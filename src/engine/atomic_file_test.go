package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRetryRename(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		src := filepath.Join(tempDir, "source.txt")
		dst := filepath.Join(tempDir, "dest.txt")

		if err := os.WriteFile(src, []byte("test"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := retryRename(src, dst); err != nil {
			t.Fatalf("retryRename failed: %v", err)
		}

		if _, err := os.Stat(dst); os.IsNotExist(err) {
			t.Fatal("destination file does not exist")
		}
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Fatal("source file still exists")
		}
	})

	t.Run("source not exists", func(t *testing.T) {
		t.Parallel()
		src := filepath.Join(tempDir, "nonexistent.txt")
		dst := filepath.Join(tempDir, "dest2.txt")

		err := retryRename(src, dst)
		if err == nil {
			t.Fatal("expected error for nonexistent source")
		}
	})
}

func TestReplaceWALFile(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		livePath := filepath.Join(tempDir, "live.dat")
		newPath := filepath.Join(tempDir, "new.dat")
		backupPath := livePath + ".bak"

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
		if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
			t.Fatal("backup file still exists")
		}
	})

	t.Run("creates backup", func(t *testing.T) {
		t.Parallel()
		livePath := filepath.Join(tempDir, "live2.dat")
		newPath := filepath.Join(tempDir, "new2.dat")

		if err := os.WriteFile(livePath, []byte("original"), 0644); err != nil {
			t.Fatal(err)
		}
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
		if _, err := os.Stat(indexPath + ".bak"); !os.IsNotExist(err) {
			t.Fatal("index backup still exists")
		}
		if _, err := os.Stat(valuePath + ".bak"); !os.IsNotExist(err) {
			t.Fatal("value backup still exists")
		}
	})

	t.Run("rollback on index failure", func(t *testing.T) {
		t.Parallel()
		indexPath := filepath.Join(tempDir, "index2.dat")
		valuePath := filepath.Join(tempDir, "value2.dat")
		tempIndexPath := filepath.Join(tempDir, "nonexistent-index.tmp")
		tempValuePath := filepath.Join(tempDir, "value2.tmp")

		if err := os.WriteFile(indexPath, []byte("original-index"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(valuePath, []byte("original-value"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tempValuePath, []byte("new-value"), 0644); err != nil {
			t.Fatal(err)
		}

		err := replaceWALFiles(indexPath, valuePath, tempIndexPath, tempValuePath)
		if err == nil {
			t.Fatal("expected error for nonexistent temp index")
		}

		indexData, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(indexData) != "original-index" {
			t.Fatalf("index should be rolled back, got %q", string(indexData))
		}

		valueData, err := os.ReadFile(valuePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(valueData) != "original-value" {
			t.Fatalf("value should be rolled back, got %q", string(valueData))
		}
	})

	t.Run("rollback on value failure", func(t *testing.T) {
		t.Parallel()
		indexPath := filepath.Join(tempDir, "index3.dat")
		valuePath := filepath.Join(tempDir, "value3.dat")
		tempIndexPath := filepath.Join(tempDir, "index3.tmp")
		tempValuePath := filepath.Join(tempDir, "nonexistent-value.tmp")

		if err := os.WriteFile(indexPath, []byte("original-index"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(valuePath, []byte("original-value"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tempIndexPath, []byte("new-index"), 0644); err != nil {
			t.Fatal(err)
		}

		err := replaceWALFiles(indexPath, valuePath, tempIndexPath, tempValuePath)
		if err == nil {
			t.Fatal("expected error for nonexistent temp value")
		}

		indexData, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(indexData) != "original-index" {
			t.Fatalf("index should be rolled back, got %q", string(indexData))
		}

		valueData, err := os.ReadFile(valuePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(valueData) != "original-value" {
			t.Fatalf("value should be rolled back, got %q", string(valueData))
		}
	})
}

func TestBackupBothFiles(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		indexPath := filepath.Join(tempDir, "index.dat")
		valuePath := filepath.Join(tempDir, "value.dat")
		indexBackup := indexPath + ".bak"
		valueBackup := valuePath + ".bak"

		if err := os.WriteFile(indexPath, []byte("index-data"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(valuePath, []byte("value-data"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := backupBothFiles(indexPath, valuePath, indexBackup, valueBackup); err != nil {
			t.Fatalf("backupBothFiles failed: %v", err)
		}

		indexBackupData, err := os.ReadFile(indexBackup)
		if err != nil {
			t.Fatal(err)
		}
		if string(indexBackupData) != "index-data" {
			t.Fatalf("index backup: expected 'index-data', got %q", string(indexBackupData))
		}

		valueBackupData, err := os.ReadFile(valueBackup)
		if err != nil {
			t.Fatal(err)
		}
		if string(valueBackupData) != "value-data" {
			t.Fatalf("value backup: expected 'value-data', got %q", string(valueBackupData))
		}

		if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
			t.Fatal("original index should be moved")
		}
		if _, err := os.Stat(valuePath); !os.IsNotExist(err) {
			t.Fatal("original value should be moved")
		}
	})
}
