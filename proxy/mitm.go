package proxy

import (
	"github.com/coyove/goflyway/pkg/logg"

	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

type bufioConn struct {
	m io.Reader
	net.Conn
}

func (c *bufioConn) Read(buf []byte) (int, error) {
	return c.m.Read(buf)
}

func (proxy *ProxyClient) manInTheMiddle(client net.Conn, host, auth string) {
	_host, _ := splitHostPort(host)
	// try self signing a cert of this host
	cert := sign(_host)
	if cert == nil {
		return
	}

	client.Write(OK_HTTP)

	go func() {

		tlsClient := tls.Server(client, &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{*cert},
		})

		if err := tlsClient.Handshake(); err != nil {
			logg.E("handshake failed: ", host, ", ", err)
			return
		}

		bufTLSClient := bufio.NewReader(tlsClient)

		for {
			var err error
			var rUrl string
			var buf []byte
			if buf, err = bufTLSClient.Peek(3); err == io.EOF || len(buf) != 3 {
				break
			}

			// switch string(buf) {
			// case "GET", "POS", "HEA", "PUT", "OPT", "DEL", "PAT", "TRA":
			// 	// good
			// default:
			// 	proxy.dialUpstreamAndBridge(&bufioConn{Conn: tlsClient, m: bufTLSClient}, host, auth, DO_CONNECT|DO_OMIT_HDR)
			// 	return
			// }

			req, err := http.ReadRequest(bufTLSClient)
			if err != nil {
				logg.E("cannot read request: ", err)
				break
			}

			// if req.Header.Get("Upgrade") == "websocket" {
			// 	reqbuf, _ := httputil.DumpRequest(req, false)
			// 	up := proxy.dialUpstreamAndBridge(&bufioConn{Conn: tlsClient, m: bufTLSClient}, host, auth, DO_CONNECT|DO_OMIT_HDR)
			// 	logg.L(up.Write(reqbuf))
			// 	return
			// }

			logg.D(req.Method, " * ", req.Host, req.RequestURI)
			req.Header.Del("Proxy-Authorization")
			req.Header.Del("Proxy-Connection")

			if !isHttpsSchema.MatchString(req.URL.String()) {
				// we can ignore 443 since it's by default
				h := req.Host
				if strings.HasSuffix(h, ":443") {
					h = h[:len(h)-4]
				}

				req.URL, err = url.Parse("https://" + h + req.URL.String())
				rUrl = req.URL.String()
			}

			resp, rkeybuf, err := proxy.encryptAndTransport(req, auth)
			if err != nil {
				logg.E("proxy pass: ", rUrl, ", ", err)
				tlsClient.Write([]byte("HTTP/1.1 500 Internal Server Error\r\n\r\n" + err.Error()))
				break
			}

			defer tryClose(resp.Body)

			resp.Header.Del("Content-Length")
			resp.Header.Set("Transfer-Encoding", "chunked")

			if strings.ToLower(resp.Header.Get("Connection")) != "upgrade" {
				resp.Header.Set("Connection", "close")
				tlsClient.Write([]byte("HTTP/1.1 " + resp.Status + "\r\n"))
			} else {
				// we don't support websocket
				tlsClient.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
				break
			}

			// buf, _ := httputil.DumpResponse(resp, true)
			_ = httputil.DumpResponse

			hdr := http.Header{}
			copyHeaders(hdr, resp.Header, proxy.GCipher, false)
			if err := hdr.Write(tlsClient); err != nil {
				logg.W("write header: ", err)
				break
			}
			if _, err = io.WriteString(tlsClient, "\r\n"); err != nil {
				logg.W("write header: ", err)
				break
			}

			iocc := proxy.GCipher.WrapIO(tlsClient, resp.Body, rkeybuf, &IOConfig{Chunked: true})
			iocc.Partial = false

			if nr, err := iocc.DoCopy(); err != nil {
				logg.E("io.wrap ", nr, "bytes: ", err)
			}
		}

		logg.D("close: ", host)
		tlsClient.Close()
	}()
}
