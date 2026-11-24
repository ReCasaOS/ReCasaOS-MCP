package service

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// Journal is the audit trail of the AI clients' tool calls: one line per call, never a body,
// a parameter value or a token value. It is safe for concurrent use.
//
// shortcut: the file is never rotated; add rotation if real boxes show it growing too large.
type Journal struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	f      *os.File
	warned bool
}

// NewJournal opens nothing yet: the file is opened, append-only, at the first line.
func NewJournal(path string) *Journal { return &Journal{path: path, now: time.Now} }

var oneLine = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")

// Write appends a line: time, token id, token name, tool, action id (or "-"), status. A
// failure is logged once and never reaches the caller.
func (j *Journal) Write(t Token, tool, action, status string) {
	line := strings.Join([]string{j.now().Format(time.RFC3339), oneLine.Replace(t.ID), oneLine.Replace(t.Name), tool, oneLine.Replace(action), status}, "\t") + "\n"

	j.mu.Lock()
	defer j.mu.Unlock()
	var err error
	if j.f == nil {
		j.f, err = os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	}
	if err == nil {
		_, err = j.f.Write([]byte(line))
	}
	if err != nil && !j.warned {
		j.warned = true
		logger.Error("the MCP journal cannot be written", zap.String("path", j.path), zap.Error(err))
	}
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}
