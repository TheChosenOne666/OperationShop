package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store persists one prototype account; Save must not modify its input.
type Store interface {
	Load() (State, error)
	Save(State) error
}

// FileStore stores a single local account in a JSON file using atomic replacement.
// It is intended for one server process, not shared or distributed storage.
type FileStore struct {
	Path string
}

// Load returns an explicit error for a missing or corrupt save; it never silently resets it.
func (f FileStore) Load() (State, error) {
	file, err := os.Open(f.Path)
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	var state State
	if err := decodeJSON(file, &state); err != nil {
		return State{}, fmt.Errorf("decode save: %w", err)
	}
	return state, nil
}

// Save flushes a temporary file before replacing the previous complete save.
func (f FileStore) Save(state State) (err error) {
	if f.Path == "" {
		return fmt.Errorf("save path is required")
	}
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create save directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".mall-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary save: %w", err)
	}
	temp := file.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := file.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close temporary save: %w", closeErr))
			}
		}
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary save: %w", removeErr))
		}
	}()
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return fmt.Errorf("encode save: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync save: %w", err)
	}
	closed = true
	if err := file.Close(); err != nil {
		return fmt.Errorf("close save: %w", err)
	}
	if err := os.Rename(temp, f.Path); err != nil {
		return fmt.Errorf("replace save: %w", err)
	}
	return nil
}
