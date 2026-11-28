package sh

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"godeep/kernel/proc"
)

// curl: an HTTP(S) client with the curl options network tests use, on the
// shell's cap.Net (like get). Pure-Go curl clones (ameshkov/gocurl, gurl,
// kurly) are standalone programs dialing through Go's net package, which
// godeep doesn't route (the network is a capability, tamago's
// net.SocketFunc stays unset); this is the same idea on net/http.
//
// Exit statuses follow curl's for the cases tests check: 3 bad URL, 6 can't
// resolve, 7 can't connect, 22 HTTP error with -f, 23 write error, 26 read
// error (-d @file), 28 timeout, 35 TLS handshake, 47 too many redirects,
// 60 certificate.

const curlUsage = `usage: curl [OPTIONS] URL
  -X METHOD          request method
  -H 'NAME: VALUE'   request header (repeatable)
  -d DATA            request body (POST; repeatable, joined with &; @VOL:PATH reads a file, without newlines)
  --data-binary DATA like -d, @VOL:PATH kept as is
  -A AGENT           User-Agent
  -u USER:PASS       basic authentication
  -I                 HEAD: print the response headers
  -i                 print the response headers before the body
  -v                 print the exchange (> request, < response, * notes)
  -o VOL:PATH        write the body to a file
  -L                 follow redirects (--max-redirs N, default 50)
  -f                 fail (status 22, no body) on HTTP 400 and up
  -s / -S            silent: no error messages / show them anyway
  -k                 don't verify the server's certificate
  -m SECONDS         time limit for the whole transfer (--max-time)
  --connect-timeout SECONDS
  -w FORMAT          print FORMAT afterwards: %{http_code} %{time_total}
                     %{time_connect} %{time_appconnect} %{time_starttransfer}
                     %{size_download} %{remote_ip} %{remote_port} %{url_effective}
                     %{num_redirects} %{content_type} %{http_version}, \n`

type curlOpts struct {
	method, url, agent, user, output, write string
	headers                                 []string
	data                                    []string
	hasData                                 bool
	head, include, verbose, follow, fail    bool
	silent, showErr, insecure               bool
	maxRedirs                               int
	maxTime, connectTimeout                 time.Duration
}

func parseCurl(args []string) (*curlOpts, error) {
	o := &curlOpts{maxRedirs: 50}
	value := func(i *int, name string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("option %s: requires an argument", name)
		}
		*i++
		return args[*i], nil
	}
	seconds := func(s, name string) (time.Duration, error) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 {
			return 0, fmt.Errorf("option %s: bad number of seconds %q", name, s)
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		var v string
		switch {
		case a == "-X" || a == "--request":
			o.method, err = value(&i, a)
		case a == "-H" || a == "--header":
			v, err = value(&i, a)
			o.headers = append(o.headers, v)
		case a == "-d" || a == "--data" || a == "--data-binary" || a == "--data-raw":
			v, err = value(&i, a)
			if a == "-d" || a == "--data" {
				v = "@-strip:" + v // decided when the file is read
			}
			o.data = append(o.data, v)
			o.hasData = true
		case a == "-A" || a == "--user-agent":
			o.agent, err = value(&i, a)
		case a == "-u" || a == "--user":
			o.user, err = value(&i, a)
		case a == "-o" || a == "--output":
			o.output, err = value(&i, a)
		case a == "-w" || a == "--write-out":
			o.write, err = value(&i, a)
		case a == "--max-redirs":
			v, err = value(&i, a)
			if err == nil {
				if o.maxRedirs, err = strconv.Atoi(v); err != nil {
					err = fmt.Errorf("option %s: bad number %q", a, v)
				}
			}
		case a == "-m" || a == "--max-time":
			if v, err = value(&i, a); err == nil {
				o.maxTime, err = seconds(v, a)
			}
		case a == "--connect-timeout":
			if v, err = value(&i, a); err == nil {
				o.connectTimeout, err = seconds(v, a)
			}
		case a == "--head":
			o.head = true
		case a == "--include":
			o.include = true
		case a == "--verbose":
			o.verbose = true
		case a == "--location":
			o.follow = true
		case a == "--fail":
			o.fail = true
		case a == "--silent":
			o.silent = true
		case a == "--show-error":
			o.showErr = true
		case a == "--insecure":
			o.insecure = true
		case len(a) > 1 && a[0] == '-' && a[1] != '-':
			// combined single-letter switches: -fsSL
			for _, c := range a[1:] {
				switch c {
				case 'I':
					o.head = true
				case 'i':
					o.include = true
				case 'v':
					o.verbose = true
				case 'L':
					o.follow = true
				case 'f':
					o.fail = true
				case 's':
					o.silent = true
				case 'S':
					o.showErr = true
				case 'k':
					o.insecure = true
				default:
					return nil, fmt.Errorf("option -%c: unknown or needs its own argument", c)
				}
			}
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("option %s: unknown", a)
		default:
			if o.url != "" {
				return nil, errors.New("one URL at a time")
			}
			o.url = a
		}
		if err != nil {
			return nil, err
		}
	}
	if o.url == "" {
		return nil, errors.New("no URL")
	}
	if !strings.Contains(o.url, "://") {
		o.url = "http://" + o.url // as curl does
	}
	switch {
	case o.method != "":
	case o.head:
		o.method = http.MethodHead
	case o.hasData:
		o.method = http.MethodPost
	default:
		o.method = http.MethodGet
	}
	return o, nil
}

// curlErr is a failure with curl's exit status.
type curlErr struct {
	code int
	msg  string
}

func (e *curlErr) Error() string { return fmt.Sprintf("(%d) %s", e.code, e.msg) }

// classify maps a transport error to curl's exit status.
func classify(err error) *curlErr {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var unk x509.UnknownAuthorityError
	var host x509.HostnameError
	var inv x509.CertificateInvalidError
	var ue *url.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ue) && ue.Timeout():
		return &curlErr{28, "Operation timed out: " + err.Error()}
	case errors.As(err, &dns) || strings.Contains(err.Error(), "lookup "):
		return &curlErr{6, "Could not resolve host: " + err.Error()}
	case errors.As(err, &cert) || errors.As(err, &unk) || errors.As(err, &host) || errors.As(err, &inv):
		return &curlErr{60, "SSL certificate problem: " + err.Error()}
	case strings.Contains(err.Error(), "tls: "):
		return &curlErr{35, "TLS connect error: " + err.Error()}
	case strings.Contains(err.Error(), "stopped after"):
		return &curlErr{47, "Maximum redirects followed: " + err.Error()}
	}
	return &curlErr{7, "Failed to connect: " + err.Error()}
}

func curlCmd(ctx context.Context, env Env, _ *proc.Proc, args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(env.out(), curlUsage)
			return nil
		}
	}
	o, err := parseCurl(args)
	if err != nil {
		fmt.Fprintf(env.errOut(), "curl: %v\n%s\n", err, curlUsage)
		return statusErr(2)
	}
	err = curl(ctx, env, o)
	var ce *curlErr
	if errors.As(err, &ce) {
		if !o.silent || o.showErr {
			fmt.Fprintf(env.errOut(), "curl: %s\n", ce)
		}
		return statusErr(ce.code)
	}
	return err
}

func curl(ctx context.Context, env Env, o *curlOpts) error {
	u, err := url.Parse(o.url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &curlErr{3, "URL rejected: " + o.url}
	}
	body, err := curlBody(ctx, env, o.data)
	if err != nil {
		return &curlErr{26, err.Error()}
	}
	n, err := network(ctx, env)
	if err != nil {
		return &curlErr{7, err.Error()}
	}
	if o.maxTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.maxTime)
		defer cancel()
	}
	note := func(f string, a ...any) {
		if o.verbose {
			fmt.Fprintf(env.errOut(), "* "+f+"\n", a...)
		}
	}
	var remote string
	var start, connected, tlsDone, firstByte time.Time
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			if o.connectTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, o.connectTimeout)
				defer cancel()
			}
			note("Trying %s...", addr)
			c, err := n.Dial(ctx, "tcp", addr)
			if err == nil {
				// (httptrace's ConnectDone only fires for net.Dialer)
				connected = time.Now()
				remote = c.RemoteAddr().String()
				note("Connected to %s (%s)", addr, remote)
			}
			return c, err
		},
		TLSClientConfig:   &tls.Config{Time: wallNow(env), InsecureSkipVerify: o.insecure},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	redirects := 0
	client := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !o.follow {
			return http.ErrUseLastResponse
		}
		if len(via) > o.maxRedirs {
			return fmt.Errorf("stopped after %d redirects", o.maxRedirs)
		}
		redirects++
		note("Following redirect to %s", req.URL)
		return nil
	}}

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, o.method, u.String(), rd)
	if err != nil {
		return &curlErr{3, err.Error()}
	}
	req.Header.Set("User-Agent", "curl/godeep")
	req.Header.Set("Accept", "*/*")
	if o.agent != "" {
		req.Header.Set("User-Agent", o.agent)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if o.user != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(o.user)))
	}
	for _, h := range o.headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return &curlErr{2, fmt.Sprintf("header %q: want NAME: VALUE", h)}
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if strings.EqualFold(k, "Host") {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}

	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tlsDone = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
		WroteHeaders: func() {
			if o.verbose {
				w := env.errOut()
				host := req.Host
				if host == "" {
					host = req.URL.Host
				}
				fmt.Fprintf(w, "> %s %s HTTP/1.1\n> Host: %s\n", req.Method, req.URL.RequestURI(), host)
				printHeaders(w, "> ", req.Header)
				fmt.Fprintln(w, ">")
			}
		},
	}))
	start = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return classify(err)
	}
	defer resp.Body.Close()
	status := fmt.Sprintf("%s %s", resp.Proto, resp.Status)
	if o.verbose {
		w := env.errOut()
		fmt.Fprintf(w, "< %s\n", status)
		printHeaders(w, "< ", resp.Header)
		fmt.Fprintln(w, "<")
	}
	if o.fail && resp.StatusCode >= 400 {
		return &curlErr{22, fmt.Sprintf("The requested URL returned error: %d", resp.StatusCode)}
	}

	out := env.out()
	var file io.WriteCloser
	if o.output != "" && o.output != "-" {
		w, path, err := writable(ctx, env, o.output)
		if err != nil {
			return &curlErr{23, err.Error()}
		}
		if file, err = w.Create(path); err != nil {
			return &curlErr{23, err.Error()}
		}
		out = file
	}
	if o.include || o.head {
		var hb strings.Builder
		fmt.Fprintf(&hb, "%s\n", status)
		printHeaders(&hb, "", resp.Header)
		hb.WriteString("\n")
		io.WriteString(out, hb.String())
	}
	size, err := io.Copy(out, ctxReader{ctx, resp.Body})
	if file != nil {
		if cerr := file.Close(); err == nil && cerr != nil {
			return &curlErr{23, cerr.Error()}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return &curlErr{28, "Operation timed out: " + err.Error()}
		}
		return &curlErr{56, "Failure receiving data: " + err.Error()}
	}
	end := time.Now()
	if o.write != "" {
		since := func(t time.Time) string {
			if t.IsZero() {
				return "0.000000"
			}
			return fmt.Sprintf("%.6f", t.Sub(start).Seconds())
		}
		host, port, _ := net.SplitHostPort(remote)
		vars := map[string]string{
			"http_code":          strconv.Itoa(resp.StatusCode),
			"response_code":      strconv.Itoa(resp.StatusCode),
			"time_total":         since(end),
			"time_connect":       since(connected),
			"time_appconnect":    since(tlsDone),
			"time_starttransfer": since(firstByte),
			"size_download":      strconv.FormatInt(size, 10),
			"remote_ip":          host,
			"remote_port":        port,
			"url_effective":      resp.Request.URL.String(),
			"num_redirects":      strconv.Itoa(redirects),
			"content_type":       resp.Header.Get("Content-Type"),
			"http_version":       strings.TrimPrefix(resp.Proto, "HTTP/"),
		}
		io.WriteString(env.out(), expandWriteOut(o.write, vars))
	}
	return nil
}

// curlBody joins -d/--data-binary values with &, reading @VOL:PATH files
// (-d strips CR and LF from a file, as curl does).
func curlBody(ctx context.Context, env Env, data []string) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var parts [][]byte
	for _, d := range data {
		strip := false
		if s, ok := strings.CutPrefix(d, "@-strip:"); ok {
			d, strip = s, true
		}
		if f, ok := strings.CutPrefix(d, "@"); ok {
			fsys, rel, err := openPath(ctx, env, f)
			if err != nil {
				return nil, err
			}
			b, err := fs.ReadFile(fsys, rel)
			if err != nil {
				return nil, err
			}
			if strip {
				b = bytes.ReplaceAll(bytes.ReplaceAll(b, []byte("\r"), nil), []byte("\n"), nil)
			}
			parts = append(parts, b)
			continue
		}
		parts = append(parts, []byte(d))
	}
	return bytes.Join(parts, []byte("&")), nil
}

func printHeaders(w io.Writer, prefix string, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(w, "%s%s: %s\n", prefix, k, v)
		}
	}
}

// expandWriteOut fills curl's -w variables and the \n \t \r escapes.
func expandWriteOut(f string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		switch {
		case f[i] == '\\' && i+1 < len(f):
			switch f[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(f[i+1])
			}
			i++
		case strings.HasPrefix(f[i:], "%{"):
			end := strings.IndexByte(f[i:], '}')
			if end < 0 {
				b.WriteString(f[i:])
				return b.String()
			}
			name := f[i+2 : i+end]
			if v, ok := vars[name]; ok {
				b.WriteString(v)
			}
			i += end
		default:
			b.WriteByte(f[i])
		}
	}
	return b.String()
}
