package main

import log "github.com/sirupsen/logrus"
import "hash/crc64"
import "errors"
import "encoding/gob"
import "github.com/cheggaaa/pb/v3"
import "os"
import "syscall"
import "time"
import "io"
import "io/fs"
import "path/filepath"
import "github.com/google/renameio"

type FileInfo struct {
	Size          int64
	Inode         uint64
	HeadBytesHash uint64
	TailBytesHash uint64
	FullHash      uint64
	Mtime         time.Time
	action        string
}

type FileInfoCache struct {
	Path  string              // Path to the cache file
	Files map[string]FileInfo // path strings as keys
}

func NewCache(path string) *FileInfoCache {
	files := make(map[string]FileInfo)
	if path != "" {
		handle, err := os.Open(path)
		if os.IsNotExist(err) {
			return &FileInfoCache{
				Path:  path,
				Files: files,
			}
		}
		if err != nil {
			log.Fatalf("Could not open cache file: %s", err)
		}
		defer func() {
			handle.Close()
		}()
		dec := gob.NewDecoder(handle)
		if err = dec.Decode(&files); err != nil {
			log.Error("Could not decode cache file: ", err)
			return &FileInfoCache{
				Path:  path,
				Files: files,
			}
		}
	}
	log.Debugf("Loaded %d files from cache %s", len(files), path)
	return &FileInfoCache{
		Path:  path,
		Files: files,
	}
}

func (cache FileInfoCache) AddEntry(path string, info FileInfo) {
	val, exists := cache.Files[path]
	// For any of the following conditions, throw out the existing record
	if !exists || val.Mtime.Before(info.Mtime) || val.Inode != info.Inode || val.Size != info.Size {
		cache.Files[path] = info
	} else {
		if info.HeadBytesHash != 0 {
			val.HeadBytesHash = info.HeadBytesHash
		}
		if info.TailBytesHash != 0 {
			val.TailBytesHash = info.TailBytesHash
		}
		if info.FullHash != 0 {
			val.FullHash = info.FullHash
		}
		cache.Files[path] = val
	}
}

func (cache FileInfoCache) ScanDir(path string, minSize int64, sleep time.Duration) ([]string, error) {
	var acceptedPaths []string
	var totalScanned int64
	path, _ = filepath.Abs(path)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		log.Errorf("Path %s does not exist", path)
		return nil, err
	}
	if !info.IsDir() {
		log.Errorf("Path %s is not a directory", path)
		return acceptedPaths, nil
	}
	err = filepath.WalkDir(path,
		func(subpath string, entry fs.DirEntry, err error) error {
			pathLogger := log.WithFields(log.Fields{"path": subpath})
			if err != nil {
				log.Error(err)
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			totalScanned++
			if entry.IsDir() {
				return nil
			}
			info, _ := entry.Info()
			if info.Mode()&os.ModeSymlink != 0 {
				pathLogger.Debug("Skipping symlink")
				return nil
			}
			if info.Size() < minSize {
				pathLogger.Debugf("Skipping file smaller than %d byte(s)", minSize)
				return nil
			}
			time.Sleep(sleep)
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				pathLogger.Error("Could not stat()")
				return nil
			}
			fileInfo := FileInfo{Size: info.Size(), Inode: stat.Ino, Mtime: time.Unix(int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec))}
			cache.AddEntry(subpath, fileInfo)
			acceptedPaths = append(acceptedPaths, subpath)
			return nil
		})
	if err != nil {
		log.Error(err)
		return nil, err
	}
	return acceptedPaths, nil
}

func (cache FileInfoCache) SmallHash(candidates []string, byteLen int64, sleep time.Duration) ([]string, error) {
	// For byteLen, <0 means head, >0 means tail
	// abs(byteLen) should always be small enough that fully creating the buffer each time is fine
	if len(candidates) == 0 {
		return candidates, nil
	}
	var bar *pb.ProgressBar
	var result []string
	if log.IsLevelEnabled(log.InfoLevel) {
		bar = pb.Start64(int64(len(candidates)) * abs(byteLen))
		bar.Set(pb.Bytes, true)
		defer func() {
			bar.Finish()
		}()
	}
	table := crc64.MakeTable(crc64.ECMA)
	if byteLen == 0 {
		return nil, errors.New("cannot read 0 bytes")
	}
	for _, f := range candidates {
		// Check if we even need to do anything
		if log.IsLevelEnabled(log.InfoLevel) {
			bar.Add64(abs(byteLen)) // This is a slight lie, we a) haven't read anything yet and b) might read less
		}
		info := cache.Files[f]
		if (byteLen > 0 && info.HeadBytesHash != 0) || (byteLen < 0 && info.TailBytesHash != 0) {
			result = append(result, f)
			continue
		}
		time.Sleep(sleep)
		readSize := abs(byteLen)
		seek := int64(0)
		// Limit ourselves to readonly only the whole file
		if info.Size <= readSize {
			readSize = info.Size
			seek = 0
		} else if byteLen < 0 {
			// If not whole file, and we're tailing, prepare to seek
			seek = byteLen
		}
		buffer := make([]byte, readSize)
		handle, err := os.Open(f)
		if err != nil {
			Logger(f).Errorf("Could not open file: %s", err)
			handle.Close()
			continue
		}
		if seek < 0 {
			_, err = handle.Seek(seek, 2)
			if err != nil {
				Logger(f).Errorf("Error seeking: %s", err)
				continue
			}
		}
		readTotal, err := handle.Read(buffer)
		handle.Close()
		if err != nil {
			Logger(f).Errorf("Could not read file: %s", err)
			continue
		}
		if int64(readTotal) != readSize {
			Logger(f).Error("Could not read full file")
			continue
		}
		// Check original param for head/tail
		if byteLen > 0 {
			info.HeadBytesHash = crc64.Checksum(buffer, table)
			if readSize == info.Size {
				info.TailBytesHash = info.HeadBytesHash
				info.FullHash = info.HeadBytesHash
			}
		} else {
			info.TailBytesHash = crc64.Checksum(buffer, table)
			if readSize == info.Size {
				info.HeadBytesHash = info.TailBytesHash
				info.FullHash = info.TailBytesHash
			}
		}
		cache.AddEntry(f, info)
		result = append(result, f)
	}
	return result, nil
}

func (cache FileInfoCache) FullHashFiles(candidates []string, sleep time.Duration) ([]string, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	var result []string
	var bar *pb.ProgressBar
	var barReader io.Reader
	table := crc64.MakeTable(crc64.ECMA)
	if log.IsLevelEnabled(log.InfoLevel) {
		bar = pb.Start64(totalSize(cache, candidates))
		bar.Set(pb.Bytes, true)
		defer func() {
			bar.Set("prefix", "")
			bar.Finish()
		}()
	}

	// remove candidates that are already hashed
	remaining_candidates := []string{}
	for _, f := range candidates {
		info := cache.Files[f]
		// Check if we need to even do anythong
		if info.FullHash != 0 {
			result = append(result, f)
			if log.IsLevelEnabled(log.InfoLevel) {
				bar.Add64(info.Size)
			}
			continue
		} else {
			remaining_candidates = append(remaining_candidates, f)
		}
	}

	for _, f := range remaining_candidates {
		info := cache.Files[f]
		// Check if we need to even do anythong
		if info.FullHash != 0 {
			result = append(result, f)
			if log.IsLevelEnabled(log.InfoLevel) {
				bar.Add64(info.Size)
			}
			continue
		}
		if log.IsLevelEnabled(log.InfoLevel) {
			bar.Set("prefix", filepath.Base(f+" "))
		}
		time.Sleep(sleep)
		hasher := crc64.New(table)
		handle, err := os.Open(f)
		if err != nil {
			Logger(f).Error(err)
			continue
		}
		if log.IsLevelEnabled(log.InfoLevel) {
			barReader = bar.NewProxyReader(handle)
		} else {
			barReader = handle
		}
		_, err = io.Copy(hasher, barReader)
		if err != nil {
			Logger(f).Error(err)
			continue
		}
		handle.Close()
		if err != nil {
			Logger(f).Error(err)
			continue
		}
		info.FullHash = hasher.Sum64()
		cache.AddEntry(f, info)
		_ = cache.Save() // This probably will be horrendous with lots of small files the first time around, but that's what -sleep and -minsize are for
		result = append(result, f)
	}
	return result, nil
}

func (cache FileInfoCache) Save() error {
	if cache.Path == "" {
		return nil
	}

	t, err := renameio.TempFile("", cache.Path)

	if err != nil {
		return err
	}

	defer t.Cleanup()

	enc := gob.NewEncoder(t)

	if err = enc.Encode(cache.Files); err != nil {
		log.Error("Could not encode cache data: ", err)
		return err
	}

	return t.CloseAtomicallyReplace()
}
