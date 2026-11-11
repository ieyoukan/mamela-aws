package aws_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The program in the Pome is exactly a build of this source (build.sh), and its manifest file
// names it.
func TestPomeProgramIsTheBuild(t *testing.T) {
	out := filepath.Join(t.TempDir(), "provider.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, "../cmd/aws-provider")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	built, _ := os.ReadFile(out)
	committed, err := os.ReadFile("../pome/provider.wasm")
	if err != nil || !bytes.Equal(built, committed) {
		t.Fatal("pome/provider.wasm is not a build of this source: run ./build.sh")
	}
	sum := sha256.Sum256(committed)
	toml, _ := os.ReadFile("../pome/mamela-provider.toml")
	if !strings.Contains(string(toml), `sha256 = "`+hex.EncodeToString(sum[:])+`"`) {
		t.Error("pome/mamela-provider.toml does not name the program: run ./build.sh")
	}
}
