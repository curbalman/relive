package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/stretchr/testify/require"
)

func TestAutoScan_SubtreeCompletionUpdatesParentBaseline(t *testing.T) {
	root := t.TempDir()
	svc, config := newAutoScanTestService(t, root)
	year := filepath.Join(root, "2026")
	old := filepath.Join(year, "old")
	sibling := filepath.Join(root, "2025")
	for _, dir := range []string{old, sibling} {
		require.NoError(t, os.MkdirAll(dir, 0755))
	}
	baseline, err := svc.buildScanTreeSnapshot(root)
	require.NoError(t, err)
	now := time.Now()
	writeConfigJSON(t, config, "photos.scan_paths", scanPathsConfig{Paths: []scanPathConfig{
		{ID: "root", Path: root, Enabled: true, AutoScanEnabled: boolPtr(true), LastScannedAt: &now},
		{ID: "year", Path: year, Enabled: true, AutoScanEnabled: boolPtr(true), LastScannedAt: &now},
	}})
	require.NoError(t, svc.saveScanTreeSnapshot("root", baseline))
	// A directory is removed and another added in the scanned subtree. A
	// sibling changes after discovery and must remain detectable next time.
	require.NoError(t, os.Remove(old))
	newDir := filepath.Join(year, "new")
	require.NoError(t, os.Mkdir(newDir, 0755))
	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(sibling, future, future))
	subtree, err := svc.buildScanTreeSnapshot(year)
	require.NoError(t, err)
	require.NoError(t, svc.updateScanTreeSnapshotWithSnapshot(year, subtree))
	merged, err := svc.loadScanTreeSnapshot("root")
	require.NoError(t, err)
	changed, err := svc.scanTreeChangedDirs(merged)
	require.NoError(t, err)
	require.Equal(t, []string{sibling}, changed, "completed subtree must not repeat or hide an unvisited sibling change")
	paths := map[string]bool{}
	for _, node := range merged.Nodes {
		paths[node.Path] = true
	}
	require.True(t, paths[newDir])
	require.False(t, paths[old])
	nested, err := svc.loadScanTreeSnapshot("year")
	require.NoError(t, err)
	require.Equal(t, subtree.Nodes, nested.Nodes, "all configured roots covered by the scan must update")
}

func TestAutoScan_SubtreeNoLongerRepeatsAndNewDirectoryStillDetected(t *testing.T) {
	root := t.TempDir()
	svc, config := newAutoScanTestService(t, root)
	year := filepath.Join(root, "2026")
	require.NoError(t, os.Mkdir(year, 0755))
	now := time.Now()
	writeConfigJSON(t, config, "photos.auto_scan", autoScanConfig{Enabled: true, IntervalMinutes: 60})
	writeConfigJSON(t, config, "photos.scan_paths", scanPathsConfig{Paths: []scanPathConfig{
		{ID: "root", Path: root, Enabled: true, AutoScanEnabled: boolPtr(true), LastScannedAt: &now},
	}})
	baseline, err := svc.buildScanTreeSnapshot(root)
	require.NoError(t, err)
	require.NoError(t, svc.saveScanTreeSnapshot("root", baseline))
	require.NoError(t, os.Mkdir(filepath.Join(year, "trip"), 0755))
	require.NoError(t, svc.RunAutoScanCheck())
	first := waitForTaskStatus(t, svc, map[string]bool{model.ScanJobStatusCompleted: true}, 3*time.Second)
	require.Equal(t, year, first.Path)
	svc.lastAutoScanCheck = time.Time{}
	require.NoError(t, svc.RunAutoScanCheck())
	require.Equal(t, first.ID, svc.GetScanTask().ID, "unchanged subtree should not enqueue a second scan")
	require.NoError(t, os.Mkdir(filepath.Join(year, "trip", "new"), 0755))
	svc.lastAutoScanCheck = time.Time{}
	require.NoError(t, svc.RunAutoScanCheck())
	second := waitForTaskStatus(t, svc, map[string]bool{model.ScanJobStatusCompleted: true}, 3*time.Second)
	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, filepath.Join(year, "trip"), second.Path)
}

func TestAutoScan_ParentScanPreservesTemporarilyMissingConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	svc, config := newAutoScanTestService(t, root)
	child := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(child, 0755))
	writeConfigJSON(t, config, "photos.scan_paths", scanPathsConfig{Paths: []scanPathConfig{
		{ID: "parent", Path: root}, {ID: "child", Path: child},
	}})
	before, err := svc.buildScanTreeSnapshot(child)
	require.NoError(t, err)
	require.NoError(t, svc.saveScanTreeSnapshot("child", before))
	require.NoError(t, os.Remove(child))
	parent, err := svc.buildScanTreeSnapshot(root)
	require.NoError(t, err)
	require.NoError(t, svc.updateScanTreeSnapshotWithSnapshot(root, parent))
	preserved, err := svc.loadScanTreeSnapshot("child")
	require.NoError(t, err)
	require.Equal(t, before.Nodes, preserved.Nodes)
	require.NoError(t, os.Mkdir(child, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(child, "new.jpg"), []byte("fixture"), 0644))
	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(child, future, future))
	changed, err := svc.scanTreeChangedDirs(preserved)
	require.NoError(t, err)
	require.Equal(t, []string{child}, changed, "returning configured root must remain discoverable")
}

type scanEventTrigger struct{ calls chan struct{} }

func (s *scanEventTrigger) RunIncremental()                                    { s.calls <- struct{}{} }
func (*scanEventTrigger) StartClustering() (*model.EventClusteringTask, error) { return nil, nil }
func (*scanEventTrigger) StartRebuild() (*model.EventClusteringTask, error)    { return nil, nil }
func (*scanEventTrigger) StopTask() error                                      { return nil }
func (*scanEventTrigger) GetTask() *model.EventClusteringTask                  { return nil }
func (*scanEventTrigger) StopAutoWorker()                                      {}

func TestAutoScan_OnlyPhotoChangesTriggerEvents(t *testing.T) {
	root := t.TempDir()
	svc, _ := newAutoScanTestService(t, root)
	trigger := &scanEventTrigger{calls: make(chan struct{}, 4)}
	svc.eventClusteringService = trigger
	svc.processPhotoFunc = func(path string, info os.FileInfo) (*model.Photo, error) {
		mtime := info.ModTime()
		return &model.Photo{FilePath: path, FileName: info.Name(), FileSize: info.Size(), FileModTime: &mtime, Width: 100, Height: 100}, nil
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "new.jpg"), []byte("fixture"), 0644))
	_, err := svc.StartScan(root)
	require.NoError(t, err)
	waitForTaskStatus(t, svc, map[string]bool{model.ScanJobStatusCompleted: true}, 3*time.Second)
	select {
	case <-trigger.calls:
	case <-time.After(time.Second):
		t.Fatal("new photo did not trigger clustering")
	}
	_, err = svc.StartScan(root)
	require.NoError(t, err)
	task := waitForTaskStatus(t, svc, map[string]bool{model.ScanJobStatusCompleted: true}, 3*time.Second)
	require.Zero(t, task.NewPhotos)
	require.Zero(t, task.UpdatedPhotos)
	select {
	case <-trigger.calls:
		t.Fatal("unchanged scan triggered clustering")
	case <-time.After(50 * time.Millisecond):
	}
	// New input must still wake clustering so small clusters can grow.
	require.NoError(t, os.WriteFile(filepath.Join(root, "another.jpg"), []byte("fixture"), 0644))
	_, err = svc.StartScan(root)
	require.NoError(t, err)
	waitForTaskStatus(t, svc, map[string]bool{model.ScanJobStatusCompleted: true}, 3*time.Second)
	select {
	case <-trigger.calls:
	case <-time.After(time.Second):
		t.Fatal("new input did not wake clustering")
	}
}
