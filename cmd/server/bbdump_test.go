package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBBDumpOnlyWhenFlagged(t *testing.T) {
	dir := t.TempDir()
	old := bbDumpFlag
	bbDumpFlag = filepath.Join(dir, "echo_dump_next")
	defer func() { bbDumpFlag = old }()

	// No flag: no recording.
	u := newBBUpEcho()
	if u.dump != nil {
		t.Fatal("recording without the flag")
	}
	u.close()

	// Flag: this call is recorded, the flag is gone.
	if err := os.WriteFile(bbDumpFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	u = newBBUpEcho()
	if u.dump == nil {
		t.Fatal("no recording with the flag")
	}
	if _, err := os.Stat(bbDumpFlag); !os.IsNotExist(err) {
		t.Fatal("the flag is still there")
	}
	for i := 0; i < 10; i++ {
		u.toPhone(make([]float32, 320))
		u.mic(make([]float32, 320), uint16((i+1)*320))
	}
	u.close()

	runs, _ := filepath.Glob(filepath.Join(dir, "echo_dump", "*"))
	if len(runs) != 1 {
		t.Fatalf("%d recordings, want 1", len(runs))
	}
	for _, name := range []string{"mic.raw", "ref.raw", "out.raw"} {
		st, err := os.Stat(filepath.Join(runs[0], name))
		if err != nil || st.Size() != 10*320*2 {
			t.Fatalf("%s: %v, size %v", name, err, st)
		}
	}

	// The next call isn't recorded.
	u = newBBUpEcho()
	defer u.close()
	if u.dump != nil {
		t.Fatal("recording a second call")
	}
}
