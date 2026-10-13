package platform

import (
	"bufio"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func loadNetscapeCookies(jar http.CookieJar, path string) {
	if path == "" {
		return
	}
	file, e := os.Open(path)
	if e != nil {
		return
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		line := scan.Text()
		httpOnly := strings.HasPrefix(line, "#HttpOnly_")
		if httpOnly {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		expires, _ := strconv.ParseInt(fields[4], 10, 64)
		secure := strings.EqualFold(fields[3], "TRUE")
		scheme := "http"
		if secure {
			scheme = "https"
		}
		u, e := url.Parse(scheme + "://" + strings.TrimPrefix(fields[0], ".") + fields[2])
		if e != nil {
			continue
		}
		cookie := &http.Cookie{Name: fields[5], Value: fields[6], Domain: fields[0], Path: fields[2], Secure: secure, HttpOnly: httpOnly}
		if expires > 0 {
			cookie.Expires = time.Unix(expires, 0)
		}
		jar.SetCookies(u, []*http.Cookie{cookie})
	}
}
