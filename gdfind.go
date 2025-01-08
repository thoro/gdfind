package main

import (
	"os/signal"
	"encoding/csv"
	"sort"
	"fmt"
	"os"
	"time"
	"flag"
	log "github.com/sirupsen/logrus"
)

func main() {
	var minSize, headBytes, tailBytes int64
	var ioSleep time.Duration
	var err error
	var dryRun,mergeCache bool
	var logLevel, action, output, cachePath string
	flag.Int64Var(&minSize, "minsize", 1, "Ignore files with less than N `bytes`")
	flag.Int64Var(&headBytes, "head-bytes", 64, "Read N `bytes` from the start of files")
	flag.Int64Var(&tailBytes, "tail-bytes", 64, "Read N `bytes` from the end of files")
	flag.DurationVar(&ioSleep, "sleep", time.Duration(0), "Sleep N long between IO (default 0ms)")
	flag.StringVar(&logLevel, "level", "info", "Level to use for logs [warn,debug,info,error]")
	flag.StringVar(&action, "action", "none", "Action use for handling dupes [none,hardlink,symlink,delete]")
	flag.StringVar(&output, "output", "", "Write actions to `file`")
	flag.StringVar(&cachePath, "cache", "", "Cache data to `file`. Note that changing -{head,tail}-bytes does not yyet properly invalidate the cache")
	flag.BoolVar(&dryRun, "dry-run", false, "Don't actually make any changes, just print actions")
	flag.BoolVar(&mergeCache, "merge-cache", false, "Instead of searching files, merge cache files passed directly as args. First arg is the base path for the --cache, second arg the cache to be merged, third arg the base path that should be replaced")

	flag.Parse()
	switch logLevel {
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.Warnf("Unknown log level %s", logLevel)
	}
	if flag.NArg() == 0 {
		flag.Usage()
		log.Fatal("No paths provided")
		os.Exit(1)
	}

	if mergeCache {
		if flag.NArg() != 3 {
			log.Fatal("Provide exactly one cache to merge")
			os.Exit(1)
		}

		err := merge(cachePath, flag.Arg(0), flag.Arg(1), flag.Arg(2))

		if err != nil {
			log.Fatalf("Error merging caches: %s", err)
		}
		os.Exit(0)
	}

	if dryRun {
		log.Info("Running in dry run mode")
	}

	// Initial scan
	var candidates []string      // A list of paths of potential candidates
	cache := NewCache(cachePath) // Holds the interesting file information, may not actually make it to the disk

	// Catch SIGINT, so we can save the file one last time
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func(){
	    for range c {
	        // sig is a ^C, handle it
	        err := cache.Save()
	        if err != nil {
	        	log.Fatalf("Error saving cache file: %s", err)
	        	os.Exit(1)
	        }

	        log.Info("Saved file")
	        os.Exit(0)
	    }
	}()

	for i := 0; i < flag.NArg(); i++ {
		dirCandidates, err := cache.ScanDir(flag.Arg(i), minSize, time.Duration(0))
		if err == nil {
			candidates = append(candidates, dirCandidates...)
			candidateLogger(*cache, candidates).Infof("Finished scanning '%s'", flag.Arg(i))
		}
	}
	candidateLogger(*cache, candidates).Infof("Found scanning all paths")
	candidates, _ = removeUniqueSizes(*cache, candidates)
	candidateLogger(*cache, candidates).Infof("Removed unique sizes")
	candidates, _ = removeDuplicateInodes(*cache, candidates)
	candidateLogger(*cache, candidates).Infof("Removed duplicate inodes")

	// Sort by inode
	log.Debug("Sorting by inode")
	sort.Slice(candidates, func(i, j int) bool {
		return cache.Files[candidates[i]].Inode < cache.Files[candidates[j]].Inode
	})
	candidateLogger(*cache, candidates).Info("Building head hashes")
	candidates, err = cache.SmallHash(candidates, headBytes, ioSleep)
	_ = cache.Save()
	if err != nil {
		log.Error(err)
	}
	candidates, _ = removeUniqueHeadHash(*cache, candidates)
	candidateLogger(*cache, candidates).Info("Removed unique hashes, building tail hashes")
	candidates, err = cache.SmallHash(candidates, tailBytes*-1, ioSleep)
	_ = cache.Save()
	if err != nil {
		log.Error(err)
	}
	candidates, err = removeUniqueTailHash(*cache, candidates)
	if err != nil {
		log.Fatal(err)
	}
	candidateLogger(*cache, candidates).Info("Removed unique hashes, building full hashes")
	candidates, _ = cache.FullHashFiles(candidates, ioSleep)
	if err = cache.Save(); err != nil {
		log.Error("Could not save cache: ", err)
	}
	if len(candidates) == 0 {
		log.Info("No duplicates found!")
		return
	}
	hashCandidates := make(map[uint64][]string)
	for _, f := range candidates {
		hashCandidates[cache.Files[f].FullHash] = append(hashCandidates[cache.Files[f].FullHash], f)
	}
	var actionCandidates []FileInfo
	for _, files := range hashCandidates {
		if len(files) == 1 {
			// No dupes
			continue
		}
		source := files[0]
		origin := cache.Files[source]
		log.Debugf("Path %s has %d dupe(s)", source, len(files)-1)
		origin.action = "originfile"
		actionCandidates = append(actionCandidates, origin)
		for _, target := range files[1:] {
			dupe := cache.Files[target]
			log.Debug("- ", target)
			switch action {
			case "hardlink":
				if dryRun {
					log.Debug("os.Remove('" + target + "')")
					log.Debug("os.Link('" + source + "', '" + target + "')")
					dupe.action = action + "-dry-run"
				} else {
					err = hardLink(source, target)
					if err != nil {
						dupe.action = action + "-error"
					} else {
						dupe.action = action
					}
				}
			case "none":
				dupe.action = "none"
			}
			actionCandidates = append(actionCandidates, dupe)
		}
	}
	if dryRun || action == "none" {
		size := int64(0)
		files := int64(0)
		for _, file := range actionCandidates {
			if file.action == action+"-dry-run" || file.action == "none" {
				size += file.Size
				files++
			}
		}
		log.Infof("Possible save of %d files, totalling %s", files, byteToHuman(size))
	} else {
		size := int64(0)
		files := int64(0)
		for _, file := range actionCandidates {
			if file.action == action {
				size += file.Size
				files++
			}
		}
		log.Infof("Effected %d files (%s) with action '%s'", files, byteToHuman(size), action)
	}
	if output != "" {
		var handle *os.File
		if output == "-" {
			handle = os.Stdout
		} else {
			handle, err = os.Create(output)
			if err != nil {
				log.Errorf("Could not open output file: %s", err)
				return
			}
		}
		defer handle.Close()
		writer := csv.NewWriter(handle)
		if err := writer.Write(FileInfoHeaders()); err != nil {
			log.Errorf("Error writing to output file: %s", err)
			return
		}
		writer.Flush()
		for _, file := range actionCandidates {
			if err := writer.Write(file.ToCsvSlice()); err != nil {
				log.Errorf("Error writing to output file: %s", err)
			}
		}
		writer.Flush()
	}
}

func hardLink(sourcePath string, targetPath string) error {
	targetPathTmp := targetPath + ".tmp"
	err := os.Rename(targetPath, targetPathTmp)
	if err != nil {
		log.Errorf("Could not move to temporary file: %s", err)
		return err
	}
	err = os.Link(sourcePath, targetPath)
	if err != nil {
		log.Errorf("Error linking: %s", err)
		err = os.Rename(targetPathTmp, targetPath)
		if err != nil {
			log.Errorf("Could not restore temp file: %s", err)
			return err
		}
		return err
	}
	err = os.Remove(targetPathTmp)
	if err != nil {
		log.Errorf("Error unlinking temp file: %s", err)
		return err
	}
	return nil
}

func candidateLogger(cache FileInfoCache, candidates []string) *log.Entry {
	length := len(candidates)
	size := byteToHuman(totalSize(cache, candidates))
	return log.WithFields(log.Fields{"count": length, "size": size})
}

func byteToHuman(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func removeUniqueHeadHash(cache FileInfoCache, candidates []string) ([]string, error) {
	// Remove unique head hashes
	var headHashCount = make(map[uint64]int)
	var result []string
	for _, f := range candidates {
		headHashCount[cache.Files[f].HeadBytesHash]++
	}
	for _, f := range candidates {
		if headHashCount[cache.Files[f].HeadBytesHash] == 1 {
			Logger(f).Debug("Removing unique head hash")
		} else {
			result = append(result, f)
		}
	}
	return result, nil
}

func removeUniqueTailHash(cache FileInfoCache, candidates []string) ([]string, error) {
	// Remove unique tail hashes
	var tailHashCount = make(map[uint64]int)
	var result []string
	for _, f := range candidates {
		tailHashCount[cache.Files[f].TailBytesHash]++
	}
	for _, f := range candidates {
		if tailHashCount[cache.Files[f].TailBytesHash] == 1 {
			Logger(f).Debug("Removing unique tail hash")
		} else {
			result = append(result, f)
		}
	}
	return result, nil
}

func Logger(path string) *log.Entry {
	return log.WithFields(log.Fields{"path": path})
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func totalSize(cache FileInfoCache, paths []string) int64 {
	totalSize := int64(0)
	for _, f := range paths {
		totalSize += cache.Files[f].Size
	}
	return totalSize
}

func removeDuplicateInodes(cache FileInfoCache, candidates []string) ([]string, error) {
	var countInodes = make(map[uint64]bool)
	var result []string
	for _, f := range candidates {
		inode := cache.Files[f].Inode
		if countInodes[inode] {
			Logger(f).Debug("Skipping duplicate inode")
		} else {
			countInodes[inode] = true
			result = append(result, f)
		}
	}
	return result, nil
}

func removeUniqueSizes(cache FileInfoCache, candidates []string) ([]string, error) {
	// Remove unique sizes
	var result []string
	var countSizes = make(map[int64]int)
	for _, f := range candidates {
		countSizes[cache.Files[f].Size]++
	}
	for _, f := range candidates {
		if countSizes[cache.Files[f].Size] == 1 {
			Logger(f).Debug("Skipping due to unique size")
		} else {
			result = append(result, f)
		}
	}
	return result, nil
}

func (file *FileInfo) ToCsvSlice() []string {
	return []string{fmt.Sprintf("%d", file.Size), fmt.Sprintf("%x", file.FullHash), file.action}
}

func FileInfoHeaders() []string {
	return []string{"path", "size", "hash", "action"}
}
