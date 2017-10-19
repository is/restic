package restic

import (
	"context"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/fs"
)

// Restorer2 is used to restore a snapshot to a directory.
type Restorer2 struct {
	Restorer

	workers int
	cfire   chan *restoreTask
	cback   chan *restoreTask

	dst string
	ctx context.Context
	idx *HardlinkIndex

	dirTasks  []*restoreTask
	nodeTasks []*restoreTask
}

type restoreTask struct {
	res    *Restorer2
	parent *restoreTask

	class  string
	node   *Node
	treeID ID
	dir    string

	subdir int
	child  int

	err error
}

func (task *restoreTask) sendback() {
	task.res.cback <- task
}

func (task *restoreTask) checkCompeleted() error {
	if task.class != "dir" {
		return nil
	}

	if task.child == 0 && task.subdir == 0 {
		if err := task.node.RestoreTimestamps(task.dir); err != nil {
			return err
		}

		if task.parent != nil {
			task.parent.child--
			return task.checkCompeleted()
		}
	}
	return nil
}

func (task *restoreTask) restoreNodeTo() {
	res, node, dir := task.res, task.node, task.dir
	ctx, repo, dst, idx := res.ctx, res.repo, res.dst, res.idx

	debug.Log("node %v, dir %v, dst %v", node.Name, dir, dst)
	dstPath := filepath.Join(dst, dir, node.Name)

	err := node.CreateAt(ctx, dstPath, repo, idx)
	if err != nil {
		debug.Log("node.CreateAt(%s) error %v", dstPath, err)
	}

	if err != nil && os.IsNotExist(errors.Cause(err)) {
		debug.Log("create intermediate paths")

		// Create parent directories and retry
		err = fs.MkdirAll(filepath.Dir(dstPath), 0700)
		if err == nil || os.IsExist(errors.Cause(err)) {
			err = node.CreateAt(ctx, dstPath, res.repo, idx)
		}
	}

	if err != nil {
		debug.Log("error %v", err)
		err = res.Error(dstPath, node, err)
		if err != nil {
			task.err = err
		}
	}

	debug.Log("successfully restored %v", node.Name)
	task.err = nil
}

func (task *restoreTask) run() {
	defer task.sendback()

	if task.class == "Node" {
		task.restoreNodeTo()
	}
}

// NewRestorer2 creates an extend restorer from basic restorer object.
func NewRestorer2(restorer *Restorer, workers int) *Restorer2 {
	return &Restorer2{
		Restorer: *restorer,
		workers:  workers,
	}
}

// restore worker
func restoreWorker(res *Restorer2) {
	for {
		task, ok := <-res.cfire
		if !ok {
			return
		}
		task.run()
	}
}

func newNodeTask(res *Restorer2, parent *restoreTask, dir string, node *Node) *restoreTask {
	return &restoreTask{
		res:    res,
		parent: parent,
		class:  "node",
		dir:    dir,
		node:   node,
	}
}

func newDirTask(res *Restorer2, parent *restoreTask, dir string, treeID ID) *restoreTask {
	return &restoreTask{
		res:    res,
		parent: parent,
		class:  "dir",
		dir:    dir,
		treeID: treeID,
	}
}

func (res *Restorer2) restoreDir(task *restoreTask) error {
	ctx, dst := res.ctx, res.dst
	dir, treeID := task.dir, task.treeID

	tree, err := res.repo.LoadTree(ctx, treeID)

	if err != nil {
		return res.Error(dir, nil, err)
	}

	for _, node := range tree.Nodes {
		selectedForRestore, childMayBeSelected := res.SelectFilter(filepath.Join(dir, node.Name),
			filepath.Join(dst, dir, node.Name), node)
		debug.Log("SelectFilter returned %v %v", selectedForRestore, childMayBeSelected)

		if node.Type == "dir" && childMayBeSelected {
			if node.Subtree == nil {
				return errors.Errorf("Dir without subtree in tree %v", treeID.Str())
			}

			subp := filepath.Join(dir, node.Name)
			res.addDirTask(task, subp, *node.Subtree)

			if selectedForRestore {
				mkdirTask := newNodeTask(res, nil, dir, node)
				mkdirTask.restoreNodeTo()

				if mkdirTask.err != nil {
					return mkdirTask.err
				}
			}

			task.subdir++
			continue
		}

		if selectedForRestore {
			res.addNodeTask(task, dir, node)
			task.child++
			continue
		}
	}
	return nil
}

func (res *Restorer2) restoreMain() error {
	available := res.workers
	var tasks int
	var task *restoreTask

	for {
		if available > 0 {
			tasks = len(res.nodeTasks)
			if tasks > 0 {
				task, res.nodeTasks = res.nodeTasks[tasks-1], res.nodeTasks[:tasks-1]
				res.cfire <- task
				available--
				continue
			}

			tasks = len(res.dirTasks)
			if tasks > 0 {
				task, res.dirTasks = res.dirTasks[tasks-1], res.dirTasks[:tasks-1]
				err := res.restoreDir(task)
				if err != nil {
					return err
				}
				continue
			}
		}

		if available == res.workers {
			return nil
		}

		task, ok := <-res.cback

		if !ok {
			return nil
		}
		available++

		if task.err != nil {
			return task.err
		}

		if task.parent != nil {
			if task.class == "node" {
				task.parent.child--
				if err := task.parent.checkCompeleted(); err != nil {
					return err
				}
			}
		}
	}
}

func (res *Restorer2) addNodeTask(parent *restoreTask, dir string, node *Node) *restoreTask {
	task := newNodeTask(res, parent, dir, node)
	res.nodeTasks = append(res.nodeTasks, task)
	return task
}

func (res *Restorer2) addDirTask(parent *restoreTask, dir string, treeID ID) *restoreTask {
	task := newDirTask(res, parent, dir, treeID)
	res.dirTasks = append(res.dirTasks, task)
	return task
}

// RestoreTo creates the directories and files in the snapshot below dst.
// Before an item is created, res.Filter is called.
func (res *Restorer2) RestoreTo(ctx context.Context, dst string) error {
	res.ctx = ctx
	res.idx = NewHardlinkIndex()
	res.dst = dst

	res.cfire = make(chan *restoreTask)
	res.cback = make(chan *restoreTask)

	res.dirTasks = make([]*restoreTask, 100)
	res.nodeTasks = make([]*restoreTask, 100)

	res.addDirTask(nil, string(filepath.Separator), *res.sn.Tree)

	// start worker pool
	for i := 0; i < res.workers; i++ {
		go restoreWorker(res)
	}

	err := res.restoreMain()

	close(res.cfire)
	close(res.cback)

	res.cfire = nil
	res.cback = nil

	return err
}

// Snapshot returns the snapshot this restorer is configured to use.
func (res *Restorer2) Snapshot() *Snapshot {
	return res.sn
}
