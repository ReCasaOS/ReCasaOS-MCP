package service

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
)

func TestJournalWritesOneTabSeparatedLinePerCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-calls.log")
	j := NewJournal(path)
	j.now = func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) }
	defer j.Close()

	j.Write(Token{ID: "ab12", Name: "my\tlaptop\nnew"}, "read", "GET /v2/x", "200")
	j.Write(Token{ID: "ab12", Name: "x"}, "search_actions", "-", "ok")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-11-24T12:00:00Z\tab12\tmy laptop new\tread\tGET /v2/x\t200\n" +
		"2026-11-24T12:00:00Z\tab12\tx\tsearch_actions\t-\tok\n"
	if string(raw) != want {
		t.Fatalf("%q\nwant %q", raw, want)
	}
}

// Run with -race where cgo is available.
func TestJournalLinesDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-calls.log")
	j := NewJournal(path)
	defer j.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 50; k++ {
				j.Write(Token{ID: "id", Name: strings.Repeat("n", 200)}, "read", "GET /v2/x", "200")
			}
		}()
	}
	wg.Wait()
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 1000 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if strings.Count(l, "\t") != 5 {
			t.Fatalf("broken line %q", l)
		}
	}
}

func TestJournalThatCannotBeWrittenDoesNotFail(t *testing.T) {
	logger.LogInitConsoleOnly()
	j := NewJournal(filepath.Join(t.TempDir(), "missing", "mcp-calls.log"))
	j.Write(Token{ID: "a"}, "read", "-", "error") // no panic, no error to the caller
	j.Write(Token{ID: "a"}, "read", "-", "error")
	j.Close()
}
