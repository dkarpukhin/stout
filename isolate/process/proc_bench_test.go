package process

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/net/context"

	"github.com/noxiouz/stout/isolate"
)

func BenchmarkSpawnSeq(b *testing.B) {
	spoolDir, err := ioutil.TempDir("", "example")
	if err != nil {
		b.Fatal(err)
	}
	defer os.Remove(spoolDir)

	const appName = "echo"

	executable, err := exec.LookPath(appName)
	if err != nil {
		b.Fatalf("LookPath(%s): %v", appName, err)
	}

	os.Mkdir(filepath.Join(spoolDir, appName), 0777)
	ctx := context.Background()
	box, err := NewBox(ctx, isolate.BoxConfig{"spool": spoolDir})
	if err != nil {
		b.Fatal("NewBox: ", err)
	}
	defer box.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := box.Spawn(ctx, isolate.Profile{}, appName, executable, map[string]string{}, nil)
		if err != nil {
			b.Fatal("Spawn error: ", err)
		}
		p.Kill()
	}
}

func BenchmarkSpawnParallel(b *testing.B) {
	spoolDir, err := ioutil.TempDir("", "example")
	if err != nil {
		b.Fatal(err)
	}
	defer os.Remove(spoolDir)

	const appName = "echo"

	executable, err := exec.LookPath(appName)
	if err != nil {
		b.Fatalf("LookPath(%s): %v", appName, err)
	}

	os.Mkdir(filepath.Join(spoolDir, appName), 0777)
	ctx := context.Background()
	box, err := NewBox(ctx, isolate.BoxConfig{"spool": spoolDir})
	if err != nil {
		b.Fatal("NewBox: ", err)
	}
	defer box.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p, err := box.Spawn(ctx, isolate.Profile{}, appName, executable, map[string]string{}, nil)
			if err != nil {
				b.Fatal("Spawn error: ", err)
			}
			p.Kill()
		}
	})
}
