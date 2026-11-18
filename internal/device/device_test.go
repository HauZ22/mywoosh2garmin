package device

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewVirtualIsEdge830WithFreeSerial(t *testing.T) {
	taken := map[uint32]string{}
	for i := 0; i < 200; i++ {
		id := NewVirtual(taken)
		if id.Product != DefaultProduct || id.Name != DefaultName || id.SoftwareVersion != DefaultSoftwareVersion {
			t.Fatalf("unexpected profile: %+v", id)
		}
		if id.SerialNumber < serialMin || id.SerialNumber > serialMax {
			t.Fatalf("serial %d outside range", id.SerialNumber)
		}
		if _, dup := taken[id.SerialNumber]; dup {
			t.Fatalf("serial %d reused although taken", id.SerialNumber)
		}
		taken[id.SerialNumber] = "x"
	}
}

func TestSaveLoadAndRemoveLegacy(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Load(dir); ok {
		t.Fatal("Load on empty dir should fail")
	}
	id := NewVirtual(nil)
	if err := Save(dir, id); err != nil {
		t.Fatal(err)
	}
	got, ok := Load(dir)
	if !ok || got != id {
		t.Fatalf("Load = %+v, %v; want %+v", got, ok, id)
	}

	legacy := filepath.Join(dir, legacyFile)
	if err := os.WriteFile(legacy, []byte(`{"product":4532,"serial_number":3500000001}`), 0o600); err != nil {
		t.Fatal(err)
	}
	RemoveLegacy(dir)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy device.json still present: %v", err)
	}
	if got, ok := Load(dir); !ok || got != id {
		t.Fatal("RemoveLegacy must keep the virtual device")
	}
}
