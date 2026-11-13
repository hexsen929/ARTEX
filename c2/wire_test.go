package c2

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestOperatorEnvelopeAuthenticationAndBounds(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, mode := range []string{"gcm", "cbc-hmac"} {
		t.Run(mode, func(t *testing.T) {
			e := Envelope{Key: key, Mode: mode}
			wire, err := e.Seal([]byte("fixture"), mode)
			if err != nil {
				t.Fatal(err)
			}
			plain, got, err := e.Open(wire)
			if err != nil || got != mode || string(plain) != "fixture" {
				t.Fatal(string(plain), got, err)
			}
			var m map[string]string
			json.Unmarshal(wire, &m)
			b, _ := base64.StdEncoding.DecodeString(m["d"])
			b[len(b)-1] ^= 1
			m["d"] = base64.StdEncoding.EncodeToString(b)
			bad, _ := json.Marshal(m)
			if _, _, err = e.Open(bad); err == nil {
				t.Fatal("tampering accepted")
			}
			other := Envelope{Key: bytes.Repeat([]byte{9}, 32), Mode: mode}
			if _, _, err = other.Open(wire); err == nil {
				t.Fatal("wrong key accepted")
			}
		})
	}
	if _, err := (Envelope{}).Seal(nil, "gcm"); err == nil {
		t.Fatal("missing key accepted")
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 2 {
		p = p[:2]
	}
	return w.Buffer.Write(p)
}
func TestOperatorFramingHandlesPartialWrites(t *testing.T) {
	var w shortWriter
	if err := WriteFrame(&w, []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	v, err := ReadFrame(&w)
	if err != nil || string(v) != "fixture" {
		t.Fatal(string(v), err)
	}
	if _, err = ReadFrame(bytes.NewReader([]byte{255, 255, 255, 255})); err == nil {
		t.Fatal("oversized allocation accepted")
	}
}
func TestOperatorPayloadCryptoAndExactlyOnceCommand(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	key := bytes.Repeat([]byte{3}, 32)
	for _, mode := range []string{"gcm", "cbc-hmac"} {
		t.Run(mode, func(t *testing.T) {
			script, _, err := BuildPayload(PayloadConfig{Key: base64.StdEncoding.EncodeToString(key), Token: "fixture-token", UUID: "fixture", Transport: "http", Envelope: mode, Interval: 5}, "linux")
			if err != nil {
				t.Fatal(err)
			}
			e := Envelope{Key: key, Mode: mode}
			reply, err := e.Seal([]byte("server-response"), mode)
			if err != nil {
				t.Fatal(err)
			}
			var envelope map[string]string
			json.Unmarshal(reply, &envelope)
			input, _ := json.Marshal(map[string]string{"script": script, "ciphertext": envelope["d"]})
			code := `import ast,json,sys
v=json.load(sys.stdin);tree=ast.parse(v['script']);nodes=[]
for n in tree.body:
 if isinstance(n,ast.Assign) and any(isinstance(x,ast.Name) and x.id=='MODE' for x in n.targets):break
 nodes.append(n)
ns={};exec(compile(ast.Module(nodes,[]),'payload_helpers','exec'),ns)
class Result:stdout='fixture-out';stderr='fixture-error'
class Fake:
 def __init__(self):self.calls=0
 def run(self,*args,**kwargs):self.calls+=1;return Result()
f=Fake();ns['subprocess']=f;out=ns['run']('fixture-not-executed')
print(json.dumps({'d':ns['enc'](ns['C']['key'],'client-message'),'response':ns['dec'](ns['C']['key'],v['ciphertext']),'calls':f.calls,'output':out}))
`
			cmd := exec.Command(python, "-c", code)
			cmd.Stdin = bytes.NewReader(input)
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(raw))
			}
			var got struct {
				D        string `json:"d"`
				Response string `json:"response"`
				Calls    int    `json:"calls"`
				Output   string `json:"output"`
			}
			if err = json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err, string(raw))
			}
			wire, _ := json.Marshal(map[string]string{"d": got.D})
			p, _, err := e.Open(wire)
			if err != nil || string(p) != "client-message" || got.Response != "server-response" || got.Calls != 1 || got.Output != "fixture-outfixture-error" {
				t.Fatalf("mode=%s calls=%d plain=%s response=%s err=%v", mode, got.Calls, p, got.Response, err)
			}
			if strings.Contains(script, "{{.Config}}") {
				t.Fatal("unrendered config")
			}
		})
	}
	if _, _, err = BuildPayload(PayloadConfig{Key: "bad"}, "linux"); err == nil {
		t.Fatal(fmt.Sprint("invalid key accepted"))
	}
}

func TestOperatorPowerShellEnvelopeCompatibility(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell 7 unavailable")
	}
	key := bytes.Repeat([]byte{5}, 32)
	for _, mode := range []string{"gcm", "cbc-hmac"} {
		t.Run(mode, func(t *testing.T) {
			script, _, err := BuildPayload(PayloadConfig{Key: base64.StdEncoding.EncodeToString(key), Token: "fixture", UUID: "fixture", Transport: "http", Envelope: mode, Interval: 5}, "windows")
			if err != nil {
				t.Fatal(err)
			}
			i := strings.Index(script, "while($true)")
			if i < 0 {
				t.Fatal("payload loop marker missing")
			}
			helpers := script[:i]
			wire, err := (Envelope{Key: key, Mode: mode}).Seal([]byte("server-response"), mode)
			if err != nil {
				t.Fatal(err)
			}
			var env map[string]string
			json.Unmarshal(wire, &env)
			command := helpers + "\n$ErrorActionPreference='Stop'\n$d=E $C.key 'client-message'\n$p=D $C.key '" + env["d"] + "'\n@{d=$d;response=$p}|ConvertTo-Json -Compress\n"
			raw, err := exec.Command(pwsh, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
			if err != nil {
				t.Fatal(err, string(raw))
			}
			var out struct {
				D        string `json:"d"`
				Response string `json:"response"`
			}
			if err = json.Unmarshal(bytes.TrimSpace(raw), &out); err != nil {
				t.Fatal(err, string(raw))
			}
			b, _ := json.Marshal(map[string]string{"d": out.D})
			plain, _, err := (Envelope{Key: key, Mode: mode}).Open(b)
			if err != nil || string(plain) != "client-message" || out.Response != "server-response" {
				t.Fatal(string(plain), out.Response, err)
			}
		})
	}
}
