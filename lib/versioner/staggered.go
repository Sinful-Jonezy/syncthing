// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package versioner

import (
	"path/filepath"
	"strconv"
	"time"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/sync"
	"github.com/syncthing/syncthing/lib/util"
)

func init() {
	// Register the constructor for this type of versioner with the name "staggered"
	Factories["staggered"] = NewStaggered
}

type Interval struct {
	step int64
	end  int64
}

type Staggered struct {
	versionsPath  string
	cleanInterval int64
	fs            fs.Filesystem
	interval      [4]Interval
	mutex         sync.Mutex

	stop          chan struct{}
	testCleanDone chan struct{}
}

func NewStaggered(folderID string, fs fs.Filesystem, params map[string]string) Versioner {
	maxAge, err := strconv.ParseInt(params["maxAge"], 10, 0)
	if err != nil {
		maxAge = 31536000 // Default: ~1 year
	}
	cleanInterval, err := strconv.ParseInt(params["cleanInterval"], 10, 0)
	if err != nil {
		cleanInterval = 3600 // Default: clean once per hour
	}

	versionsDir := ".stversions"

	s := &Staggered{
		versionsPath:  versionsDir,
		cleanInterval: cleanInterval,
		fs:            fs,
		interval: [4]Interval{
			{30, 3600},       // first hour -> 30 sec between versions
			{3600, 86400},    // next day -> 1 h between versions
			{86400, 592000},  // next 30 days -> 1 day between versions
			{604800, maxAge}, // next year -> 1 week between versions
		},
		mutex: sync.NewMutex(),
		stop:  make(chan struct{}),
	}

	l.Debugf("instantiated %#v", s)
	return s
}

func (v *Staggered) Serve() {
	v.clean()
	if v.testCleanDone != nil {
		close(v.testCleanDone)
	}

	tck := time.NewTicker(time.Duration(v.cleanInterval) * time.Second)
	defer tck.Stop()
	for {
		select {
		case <-tck.C:
			v.clean()
		case <-v.stop:
			return
		}
	}
}

func (v *Staggered) Stop() {
	close(v.stop)
}

func (v *Staggered) clean() {
	l.Debugln("Versioner clean: Waiting for lock on", v.versionsPath)
	v.mutex.Lock()
	defer v.mutex.Unlock()
	l.Debugln("Versioner clean: Cleaning", v.versionsPath)

	if _, err := v.fs.Stat(v.versionsPath); fs.IsNotExist(err) {
		// There is no need to clean a nonexistent dir.
		return
	}

	versionsPerFile := make(map[string][]string)
	filesPerDir := make(map[string]int)

	err := v.fs.Walk(v.versionsPath, func(path string, f fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if f.IsDir() && !f.IsSymlink() {
			filesPerDir[path] = 0
			if path != v.versionsPath {
				dir := filepath.Dir(path)
				filesPerDir[dir]++
			}
		} else {
			// Regular file, or possibly a symlink.
			ext := filepath.Ext(path)
			versionTag := filenameTag(path)
			dir := filepath.Dir(path)
			withoutExt := path[:len(path)-len(ext)-len(versionTag)-1]
			name := withoutExt + ext

			filesPerDir[dir]++
			versionsPerFile[name] = append(versionsPerFile[name], path)
		}

		return nil
	})
	if err != nil {
		l.Warnln("Versioner: error scanning versions dir", err)
		return
	}

	for _, versionList := range versionsPerFile {
		// List from filepath.Walk is sorted
		v.expire(versionList)
	}

	for path, numFiles := range filesPerDir {
		if numFiles > 0 {
			continue
		}

		if path == v.versionsPath {
			l.Debugln("Cleaner: versions dir is empty, don't delete", path)
			continue
		}

		l.Debugln("Cleaner: deleting empty directory", path)
		err = v.fs.Remove(path)
		if err != nil {
			l.Warnln("Versioner: can't remove directory", path, err)
		}
	}

	l.Debugln("Cleaner: Finished cleaning", v.versionsPath)
}

func (v *Staggered) expire(versions []string) {
	l.Debugln("Versioner: Expiring versions", versions)
	for _, file := range v.toRemove(versions, time.Now()) {
		if fi, err := v.fs.Lstat(file); err != nil {
			l.Warnln("versioner:", err)
			continue
		} else if fi.IsDir() {
			l.Infof("non-file %q is named like a file version", file)
			continue
		}

		if err := v.fs.Remove(file); err != nil {
			l.Warnf("Versioner: can't remove %q: %v", file, err)
		}
	}
}

func (v *Staggered) toRemove(versions []string, now time.Time) []string {
	var prevAge int64
	firstFile := true
	var remove []string
	for _, file := range versions {
		loc, _ := time.LoadLocation("Local")
		versionTime, err := time.ParseInLocation(TimeFormat, filenameTag(file), loc)
		if err != nil {
			l.Debugf("Versioner: file name %q is invalid: %v", file, err)
			continue
		}
		age := int64(now.Sub(versionTime).Seconds())

		// If the file is older than the max age of the last interval, remove it
		if lastIntv := v.interval[len(v.interval)-1]; lastIntv.end > 0 && age > lastIntv.end {
			l.Debugln("Versioner: File over maximum age -> delete ", file)
			err = v.fs.Remove(file)
			if err != nil {
				l.Warnf("Versioner: can't remove %q: %v", file, err)
			}
			continue
		}

		// If it's the first (oldest) file in the list we can skip the interval checks
		if firstFile {
			prevAge = age
			firstFile = false
			continue
		}

		// Find the interval the file fits in
		var usedInterval Interval
		for _, usedInterval = range v.interval {
			if age < usedInterval.end {
				break
			}
		}

		if prevAge-age < usedInterval.step {
			l.Debugln("too many files in step -> delete", file)
			remove = append(remove, file)
			continue
		}

		prevAge = age
	}

	return remove
}

// Archive moves the named file away to a version archive. If this function
// returns nil, the named file does not exist any more (has been archived).
func (v *Staggered) Archive(filePath string) error {
	l.Debugln("Waiting for lock on ", v.versionsPath)
	v.mutex.Lock()
	defer v.mutex.Unlock()

	info, err := v.fs.Lstat(filePath)
	if fs.IsNotExist(err) {
		l.Debugln("not archiving nonexistent file", filePath)
		return nil
	} else if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		panic("bug: attempting to version a symlink")
	}

	if _, err := v.fs.Stat(v.versionsPath); err != nil {
		if fs.IsNotExist(err) {
			l.Debugln("creating versions dir", v.versionsPath)
			v.fs.MkdirAll(v.versionsPath, 0755)
			v.fs.Hide(v.versionsPath)
		} else {
			return err
		}
	}

	l.Debugln("archiving", filePath)

	file := filepath.Base(filePath)
	inFolderPath := filepath.Dir(filePath)
	if err != nil {
		return err
	}

	dir := filepath.Join(v.versionsPath, inFolderPath)
	err = v.fs.MkdirAll(dir, 0755)
	if err != nil && !fs.IsExist(err) {
		return err
	}

	ver := taggedFilename(file, time.Now().Format(TimeFormat))
	dst := filepath.Join(dir, ver)
	l.Debugln("moving to", dst)
	err = osutil.Rename(v.fs, filePath, dst)
	if err != nil {
		return err
	}

	// Glob according to the new file~timestamp.ext pattern.
	pattern := filepath.Join(dir, taggedFilename(file, TimeGlob))
	newVersions, err := v.fs.Glob(pattern)
	if err != nil {
		l.Warnln("globbing:", err, "for", pattern)
		return nil
	}

	// Also according to the old file.ext~timestamp pattern.
	pattern = filepath.Join(dir, file+"~"+TimeGlob)
	oldVersions, err := v.fs.Glob(pattern)
	if err != nil {
		l.Warnln("globbing:", err, "for", pattern)
		return nil
	}

	// Use all the found filenames.
	versions := append(oldVersions, newVersions...)
	v.expire(util.UniqueStrings(versions))

	return nil
}
