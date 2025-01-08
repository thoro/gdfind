package main

import (
	"strings"
)

func merge(pathA, basePathA, pathB, basePathB string) error {
	aCache := NewCache(pathA)
	bCache := NewCache(pathB)

	for path, info := range bCache.Files {
		// replace basePathB to basePathA
		aCache.AddEntry(strings.Replace(path, basePathB, basePathA, -1), info)
	}

	return aCache.Save()
}