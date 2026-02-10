package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceWALFile(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		livePath := filepath.Join(tempDir, "live.dat")
		newPath := filepath.Join(tempDir, "new.dat")

		if err := os.WriteFile(livePath, []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(newPath, []byte("new"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := replaceWALFile(livePath, newPath); err != nil {
			t.Fatalf("replaceWALFile failed: %v", err)
		}

		data, err := os.ReadFile(livePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "new" {
			t.Fatalf("expected 'new', got %q", string(data))
		}

		if _, err := os.Stat(newPath); !os.IsNotExist(err) {
			t.Fatal("temp file still exists")
		}
	})

	t.Run("no existing file", func(t *testing.T) {
		t.Parallel()
		livePath := filepath.Join(tempDir, "live2.dat")
		newPath := filepath.Join(tempDir, "new2.dat")

		if err := os.WriteFile(newPath, []byte("updated"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := replaceWALFile(livePath, newPath); err != nil {
			t.Fatalf("replaceWALFile failed: %v", err)
		}

		data, err := os.ReadFile(livePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "updated" {
			t.Fatalf("expected 'updated', got %q", string(data))
		}
	})
}

func TestReplaceWALFiles(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("success both files", func(t *testing.T) {
		t.Parallel()
		indexPath := filepath.Join(tempDir, "index.dat")
		valuePath := filepath.Join(tempDir, "value.dat")
		tempIndexPath := filepath.Join(tempDir, "index.tmp")
		tempValuePath := filepath.Join(tempDir, "value.tmp")

		if err := os.WriteFile(indexPath, []byte("old-index"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(valuePath, []byte("old-value"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tempIndexPath, []byte("new-index"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tempValuePath, []byte("new-value"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := replaceWALFiles(indexPath, valuePath, tempIndexPath, tempValuePath); err != nil {
			t.Fatalf("replaceWALFiles failed: %v", err)
		}

		indexData, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(indexData) != "new-index" {
			t.Fatalf("index: expected 'new-index', got %q", string(indexData))
		}

		valueData, err := os.ReadFile(valuePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(valueData) != "new-value" {
			t.Fatalf("value: expected 'new-value', got %q", string(valueData))
		}

		if _, err := os.Stat(tempIndexPath); !os.IsNotExist(err) {
			t.Fatal("temp index file still exists")
		}
		if _, err := os.Stat(tempValuePath); !os.IsNotExist(err) {
			t.Fatal("temp value file still exists")
		}
	})
}
