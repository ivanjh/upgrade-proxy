package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"k8s.io/utils/env"
)

type myServerHandshaker struct {
	UpgradeType *string
}

type ProtocolError struct{ ErrorString string }

func (err *ProtocolError) Error() string { return err.ErrorString }

var (
	ErrBadRequestMethod = &ProtocolError{"bad method"}
	ErrNotUpgrade       = &ProtocolError{"not upgrade"}
)

func (c *myServerHandshaker) ReadHandshake(buf *bufio.Reader, req *http.Request) (code int, err error) {
	upgradeType := req.Header.Get("Upgrade")
	if strings.ToLower(upgradeType) == "" ||
		!strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") {
		return http.StatusBadRequest, ErrNotUpgrade
	}

	c.UpgradeType = &upgradeType
	return http.StatusSwitchingProtocols, nil
}

func (c *myServerHandshaker) AcceptHandshake(buf *bufio.Writer, resp *http.Response) (err error) {
	buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	// Rely on Upgrade & Connection headers from copied response headers

	for headerName, headerValues := range resp.Header {
		for _, headerValue := range headerValues {
			buf.WriteString(headerName + ": " + headerValue + "\r\n")
		}
	}

	buf.WriteString("\r\n")
	return buf.Flush()
}

type serverHandshaker interface {
	ReadHandshake(buf *bufio.Reader, req *http.Request) (code int, err error)
	AcceptHandshake(buf *bufio.Writer, resp *http.Response) (err error)
}

func newServerConn(inrwc io.ReadWriteCloser, inbuf *bufio.ReadWriter, inreq *http.Request) (err error) {
	fmt.Println("> [" + inreq.Method + "]" + inreq.URL.String())

	upgradeType := ""
	var hs serverHandshaker = &myServerHandshaker{UpgradeType: &upgradeType}
	_, hserr := hs.ReadHandshake(inbuf.Reader, inreq)

	var outreq *http.Request

	url := env.GetString("API_SERVER", "") + inreq.URL.String()
	outreq, err = http.NewRequest(inreq.Method, url, nil)
	if err != nil {
		return
	}

	outreq.Header = inreq.Header.Clone()
	outreq.SetBasicAuth(env.GetString("USERNAME", ""), env.GetString("PASSWORD", ""))

	var outresp *http.Response

	isUpgrade := hserr != ErrNotUpgrade

	if !isUpgrade {
		outresp, err = http.DefaultClient.Do(outreq)
		if err != nil {
			fmt.Println("Error in do:" + err.Error())
			return
		}
		fmt.Println("< [" + outresp.Status + "]" + inreq.URL.String())

		outresp.Write(inbuf)
		inbuf.Flush()

		return
	}

	fmt.Println("Inbound upgrade request. type=" + upgradeType + " url:" + inreq.URL.String())

	roundTripper := NewUpgradeRoundTripper(nil, true, false, upgradeType)
	outresp, err = roundTripper.RoundTrip(outreq)
	if err != nil {
		fmt.Println("Error in do:" + err.Error())
		return
	}
	fmt.Println("Outbound upgrade requested. url:" + inreq.URL.String())

	outConn, err := roundTripper.NewConnection(outresp)
	if err != nil {
		fmt.Println("Error in NewConnection:" + err.Error())
		return
	}

	fmt.Println("Outbound upgrade accepted. url:" + inreq.URL.String())

	//Acccept inbbound
	err = hs.AcceptHandshake(inbuf.Writer, outresp)
	if err != nil {
		code := http.StatusBadRequest
		fmt.Fprintf(inbuf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
		inbuf.WriteString("\r\n")
		inbuf.Flush()
		return
	}

	fmt.Println("Inbound upgrade accepted. url:" + inreq.URL.String())

	var allComplete sync.WaitGroup
	allComplete.Add(2)
	var anyComplete sync.Mutex
	anyComplete.Lock()

	go func() {
		io.Copy(outConn, inrwc)
		fmt.Println("Copy in to out complete. url:" + inreq.URL.String())
		anyComplete.Unlock()
		allComplete.Done()
	}()

	go func() {
		io.Copy(inrwc, outConn)
		fmt.Println("Copy out to in complete. url:" + inreq.URL.String())
		anyComplete.Unlock()
		allComplete.Done()
	}()

	anyComplete.Lock()
	fmt.Println("Calling close on in+out. url:" + inreq.URL.String())

	outConn.Close()
	inrwc.Close()
	fmt.Println("Calling close on in+out complete. url:" + inreq.URL.String())

	allComplete.Wait()
	fmt.Println("Copy out <-> in complete. url:" + inreq.URL.String())

	fmt.Println("Handling done. url:" + inreq.URL.String())
	return
}

func serveUpgrade(w http.ResponseWriter, req *http.Request) {
	rwc, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic("Hijack failed: " + err.Error())
	}
	// The server should abort the WebSocket connection if it finds
	// the client did not send a handshake that matches with protocol
	// specification.
	defer rwc.Close()
	err = newServerConn(rwc, buf, req)
	if err != nil {
		return
	}
}

func main() {
	http.HandleFunc("/", serveUpgrade)
	err := http.ListenAndServe(env.GetString("LISTEN_ADDR", ":12345"), nil)
	if err != nil {
		panic("ListenAndServe: " + err.Error())
	}
}
