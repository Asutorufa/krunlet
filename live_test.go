package krunlet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveScriptQuotes(t *testing.T) {
	script, err := makeLiveScript(Request{Command: []string{"/bin/echo", "a'b", "$(touch /tmp/bad)"}, WorkDir: "/work/it's", Env: map[string]string{"TITLE": "a'b"}}, "/.krunlet/job")
	if err != nil { t.Fatal(err) }
	for _, want := range []string{"cd '/work/it'\\''s'", "'a'\\''b'", "'$(touch /tmp/bad)'", "export TITLE='a'\\''b'"} {
		if !strings.Contains(script, want) { t.Errorf("missing %q from %q", want, script) }
	}
	if _, err := makeLiveScript(Request{Command: []string{"ok"}, Env: map[string]string{"X-Y": "bad"}}, "/x"); err == nil {
		t.Fatal("invalid environment name accepted")
	}
}

func fakeLiveHelper(t *testing.T, hang bool) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "helper")
	body := "#!/bin/sh\n" +
		"root=$(sed -n 's/.*\"RootFS\":\"\\([^\" ]*\\)\".*/\\1/p' \"$3\")\n" +
		"[ -n \"$root\" ] || exit 2\n" +
		"printf 'KRUNLET_READY\\n'\n" +
		"while IFS= read -r job; do\n" +
		"  id=${job##*/}\n" +
		"  base=${job%.sh}\n" +
		"  printf 'mock-stdout\\n' > \"$root$base.stdout\"\n" +
		"  printf 'mock-stderr\\n' > \"$root$base.stderr\"\n" +
		"  printf 'KRUNLET_DONE:%s:7\\n' \"$id\"\n" +
		"done\n"
	if hang {
		body = strings.Replace(body, "  printf 'KRUNLET_DONE:", "  sleep 5\n  printf 'KRUNLET_DONE:", 1)
	}
	if err := os.WriteFile(file, []byte(body), 0700); err != nil { t.Fatal(err) }
	return file
}

func TestPersistentVMReuse(t *testing.T) {
	vm, err := NewVM(context.Background(), Options{RootFS:t.TempDir(), HelperPath:fakeLiveHelper(t,false), Timeout:time.Second})
	if err != nil { t.Fatal(err) }
	defer vm.Close()
	for i:=0; i<2; i++ {
		r,e:=vm.Run(context.Background(), Request{Command:[]string{"/bin/echo","ok"},Files:map[string][]byte{"/work/in.txt":[]byte("hello")}})
		if e!=nil { t.Fatal(e) }
		if r.ExitCode!=7 || r.Stdout!="mock-stdout\n" || r.Stderr!="mock-stderr\n" { t.Fatalf("unexpected: %+v",r) }
	}
	file,e:=vm.session.ReadFile("/work/in.txt")
	if e!=nil || !bytes.Equal(file,[]byte("hello")) { t.Fatalf("file %q: %v",file,e) }
}

func TestPersistentVMTimeout(t *testing.T) {
	vm,err:=NewVM(context.Background(),Options{RootFS:t.TempDir(),HelperPath:fakeLiveHelper(t,true),Timeout:time.Second})
	if err!=nil {t.Fatal(err)}
	defer vm.Close()
	result,e:=vm.Run(context.Background(),Request{Command:[]string{"/bin/true"},Timeout:25*time.Millisecond})
	if !errors.Is(e,context.DeadlineExceeded) || !result.TimedOut {t.Fatalf("result %+v: %v",result,e)}
	if _,e=vm.Shell(context.Background(),"true"); e==nil {t.Fatal("reused stopped VM")}
}

func TestOneShotStreaming(t *testing.T) {
	file:=filepath.Join(t.TempDir(),"helper")
	if e:=os.WriteFile(file,[]byte("#!/bin/sh\nprintf 'hello'; printf 'world' >&2\n"),0700);e!=nil {t.Fatal(e)}
	r,e:=New(Options{RootFS:t.TempDir(),HelperPath:file})
	if e!=nil {t.Fatal(e)}
	var out,stderr bytes.Buffer
	result,e:=r.RunStream(context.Background(),Request{Command:[]string{"/bin/true"}},&out,&stderr)
	if e!=nil {t.Fatal(e)}
	if out.String()!="hello" || stderr.String()!="world" || result.Stdout!="hello" || result.Stderr!="world" {t.Fatalf("unexpected result: %+v",result)}
}
