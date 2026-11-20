package external

import (
	"sync/atomic"
	"testing"
)

func TestFoldersOfReadsTheFoldersWithTheRole(t *testing.T) {
	body := replyOf(`{"success":200,"data":"user","rights":["storage"],"folders":[{"path":"/DATA/Media","write":false},{"path":"/DATA/Kids","write":true}]}`)
	dir, hits, _, _ := rightsServer(t, body)

	got := FoldersOf(dir, "4")
	if len(got) != 2 || got[0] != (Folder{"/DATA/Media", false}) || got[1] != (Folder{"/DATA/Kids", true}) {
		t.Fatalf("folders: %v", got)
	}
	rights := RightsOf(dir, "4")
	if !rights[FilesRight] || !rights["storage"] || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("rights %v, %d requests", rights, *hits)
	}
}

func TestNoFolderMeansNoFilesRight(t *testing.T) {
	dir, _, _, _ := rightsServer(t, replyOf(`{"success":200,"data":"user","rights":["storage"]}`))

	if got := FoldersOf(dir, "4"); got == nil || len(got) != 0 {
		t.Fatalf("folders: %v", got)
	}
	if RightsOf(dir, "4")[FilesRight] {
		t.Fatal("files without a folder")
	}
}

// A folder that could widen what is reached is dropped: relative, spelled twice, the root.
func TestOnlyFoldersSpelledOnceAreKept(t *testing.T) {
	dir, _, _, _ := rightsServer(t, replyOf(`{"success":200,"data":"user","folders":[{"path":"/"},{"path":"DATA"},{"path":"/DATA/../etc"},{"path":"/DATA//x"},{"path":"/DATA/x/"},{"path":"/DATA/ok","write":true},{"path":"/DATA/ok"}]}`))

	got := FoldersOf(dir, "4")
	if len(got) != 1 || got[0] != (Folder{"/DATA/ok", true}) {
		t.Fatalf("folders: %v", got)
	}
}

// Closed: the administrator, a stale answer and a failure give no folder.
func TestFoldersOfIsClosed(t *testing.T) {
	body := replyOf(`{"success":200,"data":"admin","folders":[{"path":"/DATA"}]}`)
	dir, _, clock, _ := rightsServer(t, body)
	if len(FoldersOf(dir, "1")) != 0 {
		t.Fatal("folders for the administrator")
	}

	body.Store(`{"success":200,"data":"user","folders":[{"path":"/DATA"}]}`)
	if len(FoldersOf(dir, "4")) != 1 {
		t.Fatal("no folder for a fresh answer")
	}
	body.Store(`not json`)
	*clock = clock.Add(roleFresh)
	if len(FoldersOf(dir, "4")) != 0 {
		t.Fatal("folders from a stale answer")
	}
}

// A folder inside another leaves the mode below the inner one in doubt: no folder at all.
func TestNestedFoldersAreNone(t *testing.T) {
	dir, _, _, _ := rightsServer(t, replyOf(`{"success":200,"data":"user","folders":[{"path":"/DATA/w","write":true},{"path":"/DATA/w/ro"}]}`))
	if got := FoldersOf(dir, "4"); len(got) != 0 {
		t.Fatalf("folders: %v", got)
	}
	if RightsOf(dir, "4")[FilesRight] {
		t.Fatal("files with nested folders")
	}
}
