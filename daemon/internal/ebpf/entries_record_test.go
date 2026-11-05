package ebpf

import "testing"

func resetEntries(t *testing.T, e ...Entry) {
	t.Helper()
	Replace(e)
	t.Cleanup(func() { Replace(nil) })
}

// A sync whose listing predates a flush or rename must not throw away the key
// RecordKey recorded for it (AC-86); one taken after the event is authoritative.
func TestReplaceAppliedKeepsNewerRecordedKeys(t *testing.T) {
	resetEntries(t, Entry{Ino: 10, Name: "a", S3Key: "a", Mode: 0o100644})
	RecordKey(10, 0, "b", "b", 0, 7)     // renamed a -> b (event #7)
	RecordKey(20, 0, "new", "new", 3, 8) // created and flushed (event #8)
	RecordKey(30, 0, "old", "old", 1, 2) // an older recorded event

	// listing taken when events <= 5 were applied: it still has a, no new, and old
	ReplaceApplied([]Entry{
		{Ino: 10, Name: "a", S3Key: "a", Mode: 0o100644},
		{Ino: 30, Name: "old2", S3Key: "old2", Mode: 0o100644},
	}, 5)

	if e := LookupByIno(10); e == nil || e.S3Key != "b" || e.Name != "b" {
		t.Errorf("renamed ino after a stale listing: %+v, want key b", e)
	}
	if e := LookupByIno(20); e == nil || e.S3Key != "new" {
		t.Errorf("created ino missing from a stale listing: %+v, want it kept", e)
	}
	if e := LookupByIno(30); e == nil || e.S3Key != "old2" {
		t.Errorf("event #2 <= applied 5: the listing is authoritative, got %+v", e)
	}

	// a listing after everything was applied replaces them all
	ReplaceApplied([]Entry{{Ino: 10, Name: "b", S3Key: "b", Mode: 0o100644}}, 9)
	if LookupByIno(20) != nil {
		t.Errorf("ino 20 still there after an authoritative listing without it")
	}
}

func TestDropRecordedForgetsDeletedFile(t *testing.T) {
	resetEntries(t, Entry{Ino: 1, Name: "keep", S3Key: "keep"})
	RecordKey(2, 0, "gone", "gone", 0, 4)
	DropRecorded(2)
	DropRecorded(1) // listing entries (RecSeq 0) are not touched
	if LookupByIno(2) != nil {
		t.Errorf("recorded entry survived DropRecorded")
	}
	if LookupByIno(1) == nil {
		t.Errorf("a listing entry was dropped")
	}
}
