package service

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type Level string

const (
	LevelRead Level = "read"
	LevelFull Level = "full"
)

const (
	maxTokens      = 20
	maxNameLength  = 60
	plainPrefix    = "rcm_"
	fileVersion    = 1
	lastUsedWrites = time.Minute
)

var (
	ErrTooMany  = errors.New("too many tokens: revoke one first")
	ErrBadName  = errors.New("a token's name is 1 to 60 printable characters")
	ErrBadLevel = errors.New("a token's level is read or full")
)

// Token is what leaves the store. The hash of the plain value never does.
type Token struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Account  int       `json:"account"`
	Level    Level     `json:"level"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"` // zero: never used
}

type entry struct {
	Token
	Hash string `json:"hash"`
}

type file struct {
	Version int     `json:"version"`
	Tokens  []entry `json:"tokens"`
}

type Store struct {
	mu          sync.Mutex
	path        string
	now         func() time.Time
	entries     []entry
	lastWritten time.Time // of the last write made for a use, not for a Create or Delete
	setAside    bool
}

// OpenStore reads the file at path. A file that cannot be read as a token list is renamed to
// path+".bad" and the store starts empty, so a damaged file never stops the module.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}

	var f file
	if json.Unmarshal(raw, &f) != nil || f.Version != fileVersion {
		// Windows will not rename over a file
		_ = os.Remove(path + ".bad")
		if err := os.Rename(path, path+".bad"); err != nil {
			return nil, err
		}
		s.setAside = true

		return s, nil
	}
	s.entries = f.Tokens

	return s, nil
}

// SetAside tells whether OpenStore found a damaged file and moved it to tokens.json.bad.
func (s *Store) SetAside() bool { return s.setAside }

func hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source is gone
	}

	return b
}

func validName(name string) bool {
	if n := utf8.RuneCountInString(name); n < 1 || n > maxNameLength {
		return false
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return false
		}
	}

	return true
}

func (s *Store) Create(name string, account int, level Level) (plain string, t Token, err error) {
	name = strings.TrimSpace(name)
	if !validName(name) {
		return "", Token{}, ErrBadName
	}
	if level != LevelRead && level != LevelFull {
		return "", Token{}, ErrBadLevel
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) >= maxTokens {
		return "", Token{}, ErrTooMany
	}

	id := hex.EncodeToString(random(4))
	for s.index(id) >= 0 {
		id = hex.EncodeToString(random(4))
	}
	plain = plainPrefix + base64.RawURLEncoding.EncodeToString(random(32))
	t = Token{ID: id, Name: name, Account: account, Level: level, Created: s.now().UTC()}

	s.entries = append(s.entries, entry{Token: t, Hash: hash(plain)})
	if err := s.save(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		return "", Token{}, err
	}

	return plain, t, nil
}

func (s *Store) index(id string) int {
	for i, e := range s.entries {
		if e.ID == id {
			return i
		}
	}

	return -1
}

func (s *Store) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()

	tokens := make([]Token, len(s.entries))
	for i, e := range s.entries {
		tokens[i] = e.Token
	}

	return tokens
}

func (s *Store) Delete(id string) (t Token, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.index(id)
	if i < 0 {
		return Token{}, false
	}

	previous := s.entries
	s.entries = append(append([]entry{}, previous[:i]...), previous[i+1:]...)
	if err := s.save(); err != nil {
		// memory and file stay alike: the token is still there, and still works
		s.entries = previous
		return Token{}, false
	}

	return previous[i].Token, true
}

// Lookup finds the token a plain value belongs to and notes its use. It hashes the value once
// and compares against every entry, so its duration does not tell where a token sits.
func (s *Store) Lookup(plain string) (t Token, ok bool) {
	if !strings.HasPrefix(plain, plainPrefix) {
		return Token{}, false
	}
	candidate := []byte(hash(plain))

	s.mu.Lock()
	defer s.mu.Unlock()

	found := -1
	for i, e := range s.entries {
		if subtle.ConstantTimeCompare(candidate, []byte(e.Hash)) == 1 {
			found = i
		}
	}
	if found < 0 {
		return Token{}, false
	}

	now := s.now().UTC()
	s.entries[found].LastUsed = now
	if now.Sub(s.lastWritten) >= lastUsedWrites {
		s.lastWritten = now
		_ = s.save() // a use is not worth failing for; the next write will carry it
	}

	return s.entries[found].Token, true
}

func (s *Store) AccountHasTokens(account int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.entries {
		if e.Account == account {
			return true
		}
	}

	return false
}

// save writes the whole file beside itself, then renames it into place.
func (s *Store) save() error {
	raw, err := json.Marshal(file{Version: fileVersion, Tokens: s.entries})
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), "tokens-*.tmp") // created 0600
	if err != nil {
		return err
	}
	_, err = tmp.Write(raw)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}

	return err
}
