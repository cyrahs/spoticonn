package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicPersistenceAndRollback(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(v *State) error { v.Settings.Name = "客厅"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(v *State) error { v.Settings.Name = "broken"; return errors.New("reject") }); err == nil {
		t.Fatal("expected error")
	}
	reloaded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Settings.Name != "客厅" || reloaded.Snapshot().Settings.Name != "客厅" {
		t.Fatal("failed mutation leaked")
	}
	fi, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatal("credentials store is not private")
	}
}
func TestCorruptOrFutureStateIsNotOverwritten(t *testing.T) {
	for _, data := range []string{"not json", `{"version":99}`} {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(data), 0600)
		if _, err := Open(dir); err == nil {
			t.Fatal("invalid state accepted")
		}
		b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
		if string(b) != data {
			t.Fatal("invalid state overwritten")
		}
	}
}
