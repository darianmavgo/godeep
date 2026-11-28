package sh

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"godeep/net/hostnet"
)

func curlServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		io.WriteString(w, "hello "+r.Method+"\n")
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u, p, _ := r.BasicAuth()
		io.WriteString(w, strings.Join([]string{r.Method, string(b), r.Header.Get("Content-Type"),
			r.Header.Get("X-One"), r.UserAgent(), u + ":" + p}, "|")+"\n")
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/hello", 302) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func status(err error) int { return exitStatus(err) }

func TestCurl(t *testing.T) {
	s := newShell(t)
	s.env.Net = hostnet.Net{}
	srv := curlServer(t)
	u := srv.URL

	if got := s.must("curl " + u + "/hello"); got != "hello GET\n" {
		t.Fatalf("GET: %q", got)
	}
	if got := s.must("curl -i " + u + "/hello"); !strings.HasPrefix(got, "HTTP/1.1 200 OK\n") ||
		!strings.Contains(got, "X-Test: yes\n") || !strings.HasSuffix(got, "\n\nhello GET\n") {
		t.Fatalf("-i: %q", got)
	}
	if got := s.must("curl -I " + u + "/hello"); !strings.Contains(got, "X-Test: yes") || strings.Contains(got, "hello") {
		t.Fatalf("-I: %q", got)
	}
	got := s.must("curl -d a=1 -d b=2 -H 'X-One: 1' -A tester -u bob:pw " + u + "/echo")
	if got != "POST|a=1&b=2|application/x-www-form-urlencoded|1|tester|bob:pw\n" {
		t.Fatalf("POST: %q", got)
	}
	if got := s.must("curl -X PUT --data-binary x -H 'Content-Type: text/plain' " + u + "/echo"); !strings.HasPrefix(got, "PUT|x|text/plain|") {
		t.Fatalf("PUT: %q", got)
	}
	s.must("echo line1 > ram:/body")
	s.must("echo line2 >> ram:/body")
	if got := s.must("curl -d @ram:/body " + u + "/echo"); !strings.HasPrefix(got, "POST|line1line2|") {
		t.Fatalf("-d @file: %q", got)
	}
	if got := s.must("curl --data-binary @ram:/body " + u + "/echo"); !strings.HasPrefix(got, "POST|line1\nline2\n|") {
		t.Fatalf("--data-binary @file: %q", got)
	}

	// -f: HTTP errors are exit status 22 and print no body
	out, err := s.run("curl -sf " + u + "/missing")
	if status(err) != 22 || strings.Contains(out, "nope") {
		t.Fatalf("-f 404: status %d, %q", status(err), out)
	}
	if out, err := s.run("curl " + u + "/missing"); err != nil || out != "nope\n" {
		t.Fatalf("404 without -f: %v %q", err, out)
	}
	// redirects: not followed by default, followed with -L
	if got := s.must("curl -w '%{http_code}' " + u + "/redirect"); !strings.HasSuffix(got, "302") {
		t.Fatalf("redirect without -L: %q", got)
	}
	if got := s.must("curl -sL -w '%{http_code} %{num_redirects} %{url_effective}\\n' " + u + "/redirect"); got != "hello GET\n200 1 "+u+"/hello\n" {
		t.Fatalf("-L: %q", got)
	}
	// -o writes the body to a file
	if got := s.must("curl -s -w '%{time_connect}' " + u + "/hello"); strings.HasSuffix(got, "0.000000") {
		t.Fatalf("time_connect not measured: %q", got)
	}
	if got := s.must("curl -s -o ram:/got " + u + "/hello -w '%{size_download}'"); got != "10" {
		t.Fatalf("-o: %q", got)
	}
	if got := s.must("cat ram:/got"); got != "hello GET\n" {
		t.Fatalf("-o file: %q", got)
	}
	// -v shows the exchange
	if got := s.must("curl -v " + u + "/hello"); !strings.Contains(got, "> GET /hello HTTP/1.1") || !strings.Contains(got, "< HTTP/1.1 200 OK") {
		t.Fatalf("-v: %q", got)
	}
	// timeouts, refused connections, bad URLs and options
	if _, err := s.run("curl -s -m 0.3 " + u + "/slow"); status(err) != 28 {
		t.Fatalf("-m: status %d (%v)", status(err), err)
	}
	if _, err := s.run("curl -s http://127.0.0.1:" + freePort(t) + "/"); status(err) != 7 {
		t.Fatalf("refused: status %d (%v)", status(err), err)
	}
	if _, err := s.run("curl -s ftp://example.com/"); status(err) != 3 {
		t.Fatalf("ftp: status %d", status(err))
	}
	if _, err := s.run("curl --bogus " + u); status(err) != 2 {
		t.Fatalf("unknown option: status %d", status(err))
	}
	if _, err := s.run("restrict -net none curl -s " + u + "/hello"); err == nil {
		t.Fatal("curl worked without a network capability")
	}
}

func TestCurlTLS(t *testing.T) {
	s := newShell(t)
	s.env.Net = hostnet.Net{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure\n") }))
	defer srv.Close()
	if _, err := s.run("curl -s " + srv.URL); status(err) != 60 {
		t.Fatalf("self-signed without -k: status %d (%v)", status(err), err)
	}
	if got := s.must("curl -sk -w '%{http_code}' " + srv.URL); got != "secure\n200" {
		t.Fatalf("-k: %q", got)
	}
}

func TestExpandWriteOut(t *testing.T) {
	got := expandWriteOut(`code=%{http_code}\tt=%{time_total}\n%{unknown}%{`, map[string]string{"http_code": "200", "time_total": "0.5"})
	if got != "code=200\tt=0.5\n%{" {
		t.Fatalf("%q", got)
	}
}
