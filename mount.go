//go:build rcpud

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/knusbaum/go9p/fs"
	"github.com/knusbaum/go9p/proto"
)

// mountDir wraps a real filesystem path as a 9P directory node.
type mountDir struct {
	stat proto.Stat
	root string
}

func newMountDir(path, name, uid, gid string) *mountDir {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	mode := uint32(fi.Mode().Perm()) | proto.DMDIR
	return &mountDir{
		stat: proto.Stat{
			Name: name,
			Uid:  uid,
			Gid:  gid,
			Mode: mode,
			Muid: uid,
			Mtime: uint32(fi.ModTime().Unix()),
			Atime: uint32(time.Now().Unix()),
			Qid:  proto.Qid{Qtype: uint8(mode >> 24), Vers: 0, Uid: uint64(fi.ModTime().UnixNano())},
		},
		root: path,
	}
}

func (d *mountDir) Stat() proto.Stat             { return d.stat }
func (d *mountDir) WriteStat(s *proto.Stat) error { return fmt.Errorf("read-only") }
func (d *mountDir) SetParent(p fs.Dir)            {}
func (d *mountDir) Parent() fs.Dir               { return nil }

func (d *mountDir) Children() map[string]fs.FSNode {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil
	}
	m := make(map[string]fs.FSNode, len(entries))
	for _, e := range entries {
		fullPath := filepath.Join(d.root, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if e.IsDir() {
			m[e.Name()] = newMountDir(fullPath, fi.Name(), d.stat.Uid, d.stat.Gid)
		} else {
			m[e.Name()] = newMountFile(fullPath, fi, d.stat.Uid, d.stat.Gid)
		}
	}
	return m
}

// mountFile implements fs.File backed by a real file on disk.
type mountFile struct {
	stat  proto.Stat
	path  string
	opens map[uint64]*os.File
}

func newMountFile(path string, fi os.FileInfo, uid, gid string) *mountFile {
	return &mountFile{
		stat:  proto.Stat{
			Name: fi.Name(),
			Uid:  uid,
			Gid:  gid,
			Mode: uint32(fi.Mode().Perm()),
			Muid: uid,
			Mtime: uint32(fi.ModTime().Unix()),
			Atime: uint32(time.Now().Unix()),
			Length: uint64(fi.Size()),
			Qid:  proto.Qid{Qtype: uint8(fi.Mode().Perm() >> 24), Vers: 0, Uid: uint64(fi.ModTime().UnixNano())},
		},
		path:  path,
		opens: make(map[uint64]*os.File),
	}
}

func (f *mountFile) Stat() proto.Stat              { return f.stat }
func (f *mountFile) WriteStat(s *proto.Stat) error { return fmt.Errorf("read-only") }
func (f *mountFile) SetParent(p fs.Dir)            {}
func (f *mountFile) Parent() fs.Dir               { return nil }

func (f *mountFile) Open(fid uint64, mode proto.Mode) error {
	flags := os.O_RDONLY
	if mode&proto.Owrite != 0 {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(f.path, flags, 0)
	if err != nil {
		return err
	}
	f.opens[fid] = file
	return nil
}

func (f *mountFile) Read(fid uint64, offset uint64, count uint64) ([]byte, error) {
	file := f.opens[fid]
	if file == nil {
		return nil, fmt.Errorf("file not open")
	}
	buf := make([]byte, count)
	n, err := file.ReadAt(buf, int64(offset))
	if n > 0 {
		return buf[:n], nil
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (f *mountFile) Write(fid uint64, offset uint64, data []byte) (uint32, error) {
	file := f.opens[fid]
	if file == nil {
		return 0, fmt.Errorf("file not open")
	}
	n, err := file.WriteAt(data, int64(offset))
	return uint32(n), err
}

func (f *mountFile) Close(fid uint64) error {
	if file := f.opens[fid]; file != nil {
		delete(f.opens, fid)
		return file.Close()
	}
	return nil
}
