package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Ein Container verschwindet zwischen „docker container ls“ und „inspect“ (#66).
func TestListSkipsVanishedContainers(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "docker")
	body := `#!/bin/sh
case "$*" in
  "container ls"*) echo "aaa"; echo "bbb" ;;
  "container inspect -- aaa bbb") echo '[{"Id":"aaa","Name":"/ade-ws-1","Config":{"Labels":{"ade.workspace":"w1"}}}]'; echo "Error: No such container: bbb" >&2; exit 1 ;;
  "container inspect -- aaa") echo '[{"Id":"aaa","Name":"/ade-ws-1","Config":{"Labels":{"ade.workspace":"w1"}}}]' ;;
  "container inspect -- bbb") echo "[]"; echo "Error: No such container: bbb" >&2; exit 1 ;;
  *) echo "unerwartet: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Docker: script, DataDir: dir})
	cs, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].ID != "aaa" || cs[0].Workspace != "w1" {
		t.Fatalf("List = %+v", cs)
	}
}
