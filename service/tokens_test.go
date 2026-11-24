package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	return s, path
}

func TestCreateShowsThePlainTokenOnce(t *testing.T) {
	s, path := open(t)
	plain, tok, err := s.Create("laptop", 1, LevelRead)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "rcm_") || len(plain) != 47 {
		t.Fatalf("plain token %q, want rcm_ and 47 characters", plain)
	}
	if len(tok.ID) != 8 || tok.Account != 1 || tok.Level != LevelRead || tok.Name != "laptop" || tok.Created.IsZero() || !tok.LastUsed.IsZero() {
		t.Fatalf("token %+v", tok)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatal("the file holds the plain token")
	}
	listed, _ := json.Marshal(s.List())
	if strings.Contains(string(listed), plain) || strings.Contains(string(listed), "hash") {
		t.Fatalf("List leaks: %s", listed)
	}
}

func TestLookup(t *testing.T) {
	s, _ := open(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	plain, tok, _ := s.Create("a", 3, LevelFull)
	other, _, _ := s.Create("b", 3, LevelRead)

	now = now.Add(time.Hour)
	got, ok := s.Lookup(plain)
	if !ok || got.ID != tok.ID || got.Account != 3 || !got.LastUsed.Equal(now) {
		t.Fatalf("Lookup = %+v %v", got, ok)
	}
	for _, bad := range []string{"", "rcm_", "rcm_" + strings.Repeat("A", 43), strings.TrimPrefix(plain, "rcm_")} {
		if _, ok := s.Lookup(bad); ok {
			t.Errorf("Lookup(%q) found a token", bad)
		}
	}
	if got, ok := s.Lookup(other); !ok || got.Name != "b" {
		t.Fatalf("the other token: %+v %v", got, ok)
	}
	if _, err := s.Delete(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(plain); ok {
		t.Fatal("a deleted token still works")
	}
	if _, err := s.Delete(tok.ID); err != ErrNotFound {
		t.Fatalf("second Delete: %v, want ErrNotFound", err)
	}
}

func TestLastUsedIsWrittenOncePerMinute(t *testing.T) {
	s, path := open(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	plain, _, _ := s.Create("a", 1, LevelRead)

	onDisk := func() time.Time {
		raw, _ := os.ReadFile(path)
		var f struct{ Tokens []Token }
		if err := json.Unmarshal(raw, &f); err != nil || len(f.Tokens) != 1 {
			t.Fatalf("file: %v %s", err, raw)
		}
		return f.Tokens[0].LastUsed
	}

	s.Lookup(plain) // first use: written
	first := now
	if !onDisk().Equal(first) {
		t.Fatalf("first use not written: %v", onDisk())
	}
	now = now.Add(30 * time.Second)
	got, _ := s.Lookup(plain)
	if !got.LastUsed.Equal(now) || !onDisk().Equal(first) {
		t.Fatalf("within a minute: memory %v disk %v", got.LastUsed, onDisk())
	}
	now = now.Add(31 * time.Second)
	s.Lookup(plain)
	if !onDisk().Equal(now) {
		t.Fatalf("after a minute: disk %v, want %v", onDisk(), now)
	}
}

func TestTwentyTokensAtMost(t *testing.T) {
	s, _ := open(t)
	for i := 0; i < 20; i++ {
		if _, _, err := s.Create("t", 1, LevelRead); err != nil {
			t.Fatalf("token %d: %v", i+1, err)
		}
	}
	if _, _, err := s.Create("t", 1, LevelRead); err != ErrTooMany {
		t.Fatalf("21st: %v, want ErrTooMany", err)
	}
	if len(s.List()) != 20 {
		t.Fatal("the refused token was stored")
	}
}

func TestCreateChecksNameAndLevel(t *testing.T) {
	s, _ := open(t)
	for _, name := range []string{"", "   ", strings.Repeat("a", 61), "tab\there", "bell\a"} {
		if _, _, err := s.Create(name, 1, LevelRead); err != ErrBadName {
			t.Errorf("name %q: %v, want ErrBadName", name, err)
		}
	}
	if _, tok, err := s.Create("  "+strings.Repeat("é", 60)+"  ", 1, LevelRead); err != nil || tok.Name != strings.Repeat("é", 60) {
		t.Errorf("60 characters, padded: %v %q", err, tok.Name)
	}
	if _, _, err := s.Create("x", 1, Level("root")); err != ErrBadLevel {
		t.Errorf("level root: %v, want ErrBadLevel", err)
	}
	if _, _, err := s.Create("x", 1, ""); err != ErrBadLevel {
		t.Errorf("empty level: %v, want ErrBadLevel", err)
	}
}

func TestAccountHasTokens(t *testing.T) {
	s, _ := open(t)
	_, tok, _ := s.Create("a", 2, LevelRead)
	if !s.AccountHasTokens(2) || s.AccountHasTokens(3) {
		t.Fatal("AccountHasTokens is wrong")
	}
	_, _ = s.Delete(tok.ID)
	if s.AccountHasTokens(2) {
		t.Fatal("still has tokens after Delete")
	}
}

func TestFileIsPrivateAndWhole(t *testing.T) {
	s, path := open(t)
	var ids []string
	for i := 0; i < 3; i++ {
		plain, tok, _ := s.Create("t", 1, LevelRead)
		ids = append(ids, tok.ID)
		s.Lookup(plain)
	}
	_, _ = s.Delete(ids[0])

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v, want 0600", info, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 || entries[0].Name() != "tokens.json" {
		t.Fatalf("left beside the file: %v", entries)
	}
	reopened, err := OpenStore(path)
	if err != nil || len(reopened.List()) != 2 || len(reopened.Notes()) != 0 {
		t.Fatalf("reopen: %v, %d tokens", err, len(reopened.List()))
	}
}

func TestUnreadableFileIsSetAside(t *testing.T) {
	for name, content := range map[string]string{
		"cut":             `{"version":1,"tokens":[{"id":"ab`,
		"wrong shape":     `["a","b"]`,
		"unknown version": `{"version":2,"tokens":[]}`,
	} {
		path := filepath.Join(t.TempDir(), "tokens.json")
		if err := os.WriteFile(path+".bad", []byte("older"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := OpenStore(path)
		if err != nil || len(s.List()) != 0 || len(s.Notes()) != 1 {
			t.Fatalf("%s: %v, %d tokens, notes %v", name, err, len(s.List()), s.Notes())
		}
		if got, _ := os.ReadFile(path + ".bad"); string(got) != content {
			t.Errorf("%s: .bad holds %q", name, got)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: the bad file is still in place", name)
		}
		if _, _, err := s.Create("x", 1, LevelRead); err != nil {
			t.Errorf("%s: the store does not work after: %v", name, err)
		}
	}
}

func TestMissingFileIsAnEmptyStore(t *testing.T) {
	s, path := open(t)
	if len(s.List()) != 0 || len(s.Notes()) != 0 {
		t.Fatal("not empty")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("opening wrote a file")
	}
}

func TestConcurrentUse(t *testing.T) {
	s, _ := open(t)
	plain, _, _ := s.Create("fixed", 1, LevelRead)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s.Lookup(plain)
				if _, tok, err := s.Create("n", 2, LevelFull); err == nil {
					_, _ = s.Delete(tok.ID)
				}
				s.List()
				s.AccountHasTokens(2)
			}
		}()
	}
	wg.Wait()
	if _, ok := s.Lookup(plain); !ok {
		t.Fatal("the fixed token was lost")
	}
}

func TestDeleteThatCannotBeSavedKeepsTheToken(t *testing.T) {
	s, path := open(t)
	plain, tok, _ := s.Create("a", 1, LevelRead)

	s.path = filepath.Join(filepath.Dir(path), "gone", "tokens.json") // no directory: no temp file
	if _, err := s.Delete(tok.ID); err == nil || err == ErrNotFound {
		t.Fatalf("Delete = %v, want the save's error", err)
	}
	if _, ok := s.Lookup(plain); !ok || len(s.List()) != 1 {
		t.Fatal("the token is gone though the revocation was not saved")
	}
}

func TestUnreadableFileThatCannotBeSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.Mkdir(path, 0o700); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil || len(s.List()) != 0 || len(s.Notes()) != 1 {
		t.Fatalf("OpenStore: %v, %d tokens, notes %v", err, len(s.List()), s.Notes())
	}
	if _, err := os.Stat(path + ".bad"); err != nil && !strings.Contains(s.Notes()[0], "could not be set aside") {
		t.Fatalf("neither set aside nor said so: %v", s.Notes())
	}
}
