// Copyright 2020 Baidu, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package bcache

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
)

import (
	"utils"
)

const (
	BITMAP_LENGTH = 64
)

type DLTGroup struct {
	lock      sync.Mutex
	cond      *sync.Cond
	hasWiated int32
	dlt       *DLT

	group            *Group
	readedFilesReal  *utils.BitMap // bitmap 1 or 0
	readedFilesCache *utils.BitMap // bitmap 1 or 0

	unreadFileNum     uint32
	unreadFilesIndexs []uint32 // don't contain cached files
	unreadFiles       []uint32

	readedCachedFileNum uint32 // don't contail unreaded files
	readedCachedFiles   []uint32

	prereadFileNum uint32

	cachedFileNum     uint32   // don't contail unreaded files
	cacheedFilesCache []uint32 // index to cachedFiles
	cachedFiles       []uint32 // value is file id
}

type DLT struct {
	id            uint32 // id of DLT
	fileNum       uint32
	dataset       *Dataset
	readedFileNum uint32
	groups        []DLTGroup
	inited        bool
}

func (t *DLT) init(dataset *Dataset) error {
	var (
		err error
	)

	t.groups = make([]DLTGroup, dataset.groupNum)
	for i := uint32(0); i < dataset.groupNum; i++ {
		g := &(t.groups[i])

		// init condition variable
		g.cond = sync.NewCond(&g.lock)
		g.dlt = t

		g.group = &(dataset.groups[i])
		g.readedFilesReal, err = utils.NewBitMap(g.group.fileNum)
		if err != nil {
			return err
		}

		g.readedFilesCache, err = utils.NewBitMap(g.group.fileNum)
		if err != nil {
			return err
		}

		g.unreadFileNum = g.group.fileNum
		g.unreadFilesIndexs = make([]uint32, g.group.fileNum)
		g.unreadFiles = make([]uint32, g.group.fileNum)

		g.readedCachedFiles = make([]uint32, g.group.fileNum)

		g.cacheedFilesCache = make([]uint32, g.group.fileNum)
		g.cachedFiles = make([]uint32, g.group.fileNum)
	}
	t.inited = true

	return nil
}

func (t *DLT) Get(fileName string) (*FileNode, ErrorCode, error) {
	var (
		node *FileNode
		err  error
	)

	if !t.inited {
		return nil, CODE_DLT_NOT_INIT, nil
	}

	// get file id
	fileId, ok := t.dataset.GetFileId(fileName)
	if !ok {
		return nil, CODE_NOT_FOUND, nil
	}

	// get group id
	groupId := t.dataset.fileIdToGroups[fileId]
	group := &t.groups[groupId] // get DLT group

	// add lock
	lockReleased := false
	group.lock.Lock()
	defer func() {
		if !lockReleased {
			group.lock.Unlock()
		}
	}()

	// have been readed?
	if group.readedFilesReal.Get(fileId) {
		return nil, CODE_EMPTY, fmt.Errorf("read angin %d", fileId)
	}

	// have cache, read from cache
	if group.cachedFileNum > 0 {
		// try to read from cache
		node, err = t.getFileFromCache(fileId, group)
		if err != nil {
			return nil, CODE_EMPTY, err
		} else if node == nil {
			return nil, CODE_EMPTY,
				fmt.Errorf("don't have cache, but cachedFileNum is %d", group.cachedFileNum)
		}
		// mark file has been readed
		group.readedFilesCache.Set(node.fileId)
		// mark this file have been read (maybe replaceed by other file)
		group.readedFilesReal.Set(fileId)

		return node, CODE_OK, nil
	}

	// have unreaded file, try to read it directly
	if group.unreadFileNum > 0 {
		// don't have cache
		tempId := fileId
		if group.readedFilesCache.Get(fileId) { // maybe replaced
			tempId, ok = group.getRandomUnreadedFile()
			if !ok {
				return nil, CODE_EMPTY,
					fmt.Errorf("don't have unreaded file, but unreadFileNum is %d", group.unreadFileNum)
			}
		}

		group.readedFilesCache.Set(tempId)
		// mark this file have been read (maybe replaceed by other file)
		group.readedFilesReal.Set(fileId)

		// remove from unread files
		index := group.unreadFilesIndexs[tempId]
		if index != 0 {
			lastId := group.unreadFileNum - 1
			if index != lastId {
				group.unreadFiles[index] = group.unreadFiles[lastId]
				group.unreadFilesIndexs[group.unreadFiles[lastId]] = index
			}
			// mark this file is readed
			group.unreadFilesIndexs[tempId] = 0
			group.unreadFileNum--
		}

		// try to read from endpoint

		lockReleased = true
		group.lock.Unlock()
		node, err = readFromBackend(t.dataset, t.id, t.dataset.id, group.group.id, tempId)

		// maybe , we can cache all files
		if err != nil {
			return nil, CODE_EMPTY, err
		}

		// try add file to cache
		if group.group.allowCacheSize >= group.group.cachedSize {
			lockReleased = false
			group.lock.Lock()
			t.dataset.cachedFiles[tempId] = node
			group.readedCachedFiles[group.readedCachedFileNum] = tempId
			group.readedCachedFileNum++
		}
		return node, CODE_EMPTY, err
	}

	// waiting for preread
	if group.prereadFileNum > 0 {
		atomic.AddInt32(&group.hasWiated, 1)
		group.cond.Wait()
		atomic.AddInt32(&group.hasWiated, -1)
		// try to read from cache
		node, err = t.getFileFromCache(fileId, group)
		if err != nil {
			return nil, CODE_EMPTY, err
		} else if node == nil {
			return nil, CODE_EMPTY, fmt.Errorf("don't have cache, but get condition")
		}
		// mark file has been readed
		group.readedFilesCache.Set(node.fileId)
		// mark this file have been read (maybe replaceed by other file)
		group.readedFilesReal.Set(fileId)
		return node, CODE_EMPTY, err
	}

	return nil, CODE_EMPTY, fmt.Errorf("don't have unread file, when try to read %d", fileId)
}

// for batfs
func (t *DLT) getFileFromCache(fileId uint32, group *DLTGroup) (*FileNode, error) {
	var (
		ok bool
	)

	tempId := fileId

	if group.readedFilesCache.Get(fileId) { // have been replace, return a random file
		tempId, ok = group.getRandomCachedFile()
		if !ok {
			return nil, fmt.Errorf("can't get cached file")
		}
	} else if val := group.cacheedFilesCache[tempId]; val == 0 { // this file hasn't been cached
		tempId, ok = group.getRandomCachedFile()
		if !ok {
			return nil, fmt.Errorf("can't get cached file")
		}
	} else {
		group.markCachedFileReaded(fileId, val-1)
	}

	node := t.dataset.cachedFiles[tempId]

	if node == nil {
		return nil, fmt.Errorf("file %d is mark as cached , but not cached", tempId)
	}

	return node, nil
}

// must have enough cache
func (g *DLTGroup) getRandomCachedFile() (uint32, bool) {
	randNum := rand.Uint32() % g.cachedFileNum
	fileId := g.cachedFiles[randNum]
	g.markCachedFileReaded(fileId, randNum)
	return fileId, true
}

// mark file is readed
func (g *DLTGroup) markCachedFileReaded(fileId, index uint32) {
	g.readedCachedFiles[g.readedCachedFileNum] = fileId
	g.readedCachedFileNum++

	// move last cahce Id, fill in gap
	lastId := g.cachedFileNum - 1

	if index != lastId {
		g.cachedFiles[index] = g.cachedFiles[lastId]
		g.cacheedFilesCache[g.cachedFiles[lastId]] = (index + 1)
	}

	// mark this file is readed
	g.cacheedFilesCache[fileId] = 0
	g.cachedFileNum--
}

// on condition that cache is empty
func (g *DLTGroup) getRandomUnreadedFile() (uint32, bool) {
	if g.unreadFileNum == 0 {
		return 0, false
	}

	randNum := rand.Uint32() % g.unreadFileNum
	fileId := g.unreadFiles[randNum]

	// move last cahce Id, fill in gap
	lastId := g.unreadFileNum - 1
	if randNum != lastId {
		g.unreadFiles[randNum] = g.unreadFiles[lastId]
		g.unreadFilesIndexs[g.unreadFiles[lastId]] = (randNum + 1)
	}

	// mark this file is readed
	g.unreadFilesIndexs[fileId] = 0

	g.unreadFileNum--

	return fileId, true
}

// add file to unread files
func (g *DLTGroup) addUnreadedFile(fileId uint32) {
	index := g.unreadFilesIndexs[fileId]
	if index != 0 {
		return
	}

	if g.readedFilesCache.Get(fileId) {
		return
	}

	if g.cacheedFilesCache[fileId] != 0 {
		return
	}

	g.prereadFileNum--

	g.unreadFiles[g.unreadFileNum] = fileId
	g.unreadFileNum++
	g.unreadFilesIndexs[fileId] = g.unreadFileNum
}

// must be protect by lock
func (g *DLTGroup) addFileToCache(node *FileNode) {
	fileId := node.fileId
	// if this file has been readed
	// add to readed cache
	if g.readedFilesCache.Get(fileId) {
		fmt.Println("Wraning: %d shouldn't be read", fileId)
		if g.readedCachedFiles[fileId] == 0 {
			g.readedCachedFiles[fileId] = g.readedCachedFileNum
			g.readedCachedFileNum++
		} else {
			fmt.Println("Wraning: %d shouldn't be cacehd", fileId)
		}
		return
	}

	// remove this file from unreaded files
	orgIndex := g.unreadFilesIndexs[fileId]
	if orgIndex != 0 { // in unread file list
		if orgIndex != g.unreadFileNum {
			g.unreadFiles[orgIndex-1] = g.unreadFiles[g.unreadFileNum-1]
		}
		g.unreadFileNum--
		g.unreadFilesIndexs[fileId] = 0

	} else if g.cacheedFilesCache[fileId] == 0 { // not cached
		g.prereadFileNum--
		g.cachedFiles[g.cachedFileNum] = fileId
		g.cachedFileNum++
		g.cacheedFilesCache[fileId] = g.cachedFileNum

		if atomic.LoadInt32(&g.hasWiated) > 0 {
			g.cond.Signal()
		}
	} else { // have been cached
		fmt.Println("Wraning: file %d has been cached", fileId)
	}
}
