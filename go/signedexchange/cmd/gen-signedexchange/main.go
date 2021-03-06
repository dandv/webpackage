package main

import (
	"crypto"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/WICG/webpackage/go/signedexchange"
	"github.com/WICG/webpackage/go/signedexchange/version"
)

type headerArgs []string

func (h *headerArgs) String() string {
	return fmt.Sprintf("%v", *h)
}

func (h *headerArgs) Set(value string) error {
	*h = append(*h, value)
	return nil
}

var (
	flagUri            = flag.String("uri", "https://example.com/index.html", "The URI of the resource represented in the exchange")
	flagVersion        = flag.String("version", "1b2", "The signedexchange version")
	flagResponseStatus = flag.Int("status", 200, "The status of the response represented in the exchange")
	flagContent        = flag.String("content", "index.html", "Source file to be used as the exchange payload")
	flagCertificate    = flag.String("certificate", "cert.pem", "Certificate chain PEM file of the origin")
	flagCertificateUrl = flag.String("certUrl", "https://example.com/cert.msg", "The URL where the certificate chain is hosted at.")
	flagValidityUrl    = flag.String("validityUrl", "https://example.com/resource.validity.msg", "The URL where resource validity info is hosted at.")
	flagPrivateKey     = flag.String("privateKey", "cert-key.pem", "Private key PEM file of the origin")
	flagMIRecordSize   = flag.Int("miRecordSize", 4096, "The record size of Merkle Integrity Content Encoding")
	flagDate           = flag.String("date", "", "The datetime for the signed exchange in RFC3339 format (2006-01-02T15:04:05Z07:00). Use now by default.")
	flagExpire         = flag.Duration("expire", 1*time.Hour, "The expire time of the signed exchange")

	flagDumpSignatureMessage = flag.String("dumpSignatureMessage", "", "Dump signature message bytes to a file for debugging.")
	flagDumpHeadersCbor      = flag.String("dumpHeadersCbor", "", "Dump metadata and headers encoded as a canonical CBOR to a file for debugging.")
	flagOutput               = flag.String("o", "out.sxg", "Signed exchange output file")

	flagRequestHeader  = headerArgs{}
	flagResponseHeader = headerArgs{}
)

func init() {
	flag.Var(&flagRequestHeader, "requestHeader", "Request header arguments")
	flag.Var(&flagResponseHeader, "responseHeader", "Response header arguments")
}

func run() error {
	payload, err := ioutil.ReadFile(*flagContent)
	if err != nil {
		return fmt.Errorf("failed to read content from payload source file \"%s\". err: %v", *flagContent, err)
	}

	certtext, err := ioutil.ReadFile(*flagCertificate)
	if err != nil {
		return fmt.Errorf("failed to read certificate file %q. err: %v", *flagCertificate, err)

	}
	certs, err := signedexchange.ParseCertificates(certtext)
	if err != nil {
		return fmt.Errorf("failed to parse certificate file %q. err: %v", *flagCertificate, err)
	}

	certUrl, err := url.Parse(*flagCertificateUrl)
	if err != nil {
		return fmt.Errorf("failed to parse certificate URL %q. err: %v", *flagCertificateUrl, err)
	}
	validityUrl, err := url.Parse(*flagValidityUrl)
	if err != nil {
		return fmt.Errorf("failed to parse validity URL %q. err: %v", *flagValidityUrl, err)
	}

	privkeytext, err := ioutil.ReadFile(*flagPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to read private key file %q. err: %v", *flagPrivateKey, err)
	}
	ver, ok := version.Parse(*flagVersion)
	if !ok {
		return fmt.Errorf("failed to parse version %q", *flagVersion)
	}

	var privkey crypto.PrivateKey
	for {
		var pemBlock *pem.Block
		pemBlock, privkeytext = pem.Decode(privkeytext)
		if pemBlock == nil {
			return fmt.Errorf("invalid PEM block in private key file %q.", *flagPrivateKey)
		}

		var err error
		privkey, err = signedexchange.ParsePrivateKey(pemBlock.Bytes)
		if err == nil || len(privkeytext) == 0 {
			break
		}
		// Else try next PEM block.
	}
	if privkey == nil {
		return fmt.Errorf("failed to parse private key file %q.", *flagPrivateKey)
	}

	var fMsg io.WriteCloser
	if *flagDumpSignatureMessage != "" {
		var err error
		fMsg, err = os.Create(*flagDumpSignatureMessage)
		if err != nil {
			return fmt.Errorf("failed to open signature message dump output file %q for writing. err: %v", *flagDumpSignatureMessage, err)
		}
		defer fMsg.Close()
	}
	var fHdr io.WriteCloser
	if *flagDumpHeadersCbor != "" {
		var err error
		fHdr, err = os.Create(*flagDumpHeadersCbor)
		if err != nil {
			return fmt.Errorf("failed to open signedheaders dump output file %q for writing. err: %v", *flagDumpHeadersCbor, err)
		}
		defer fHdr.Close()
	}

	f, err := os.Create(*flagOutput)
	if err != nil {
		return fmt.Errorf("failed to open output file %q for writing. err: %v", *flagOutput, err)
	}
	defer f.Close()

	parsedUrl, err := url.Parse(*flagUri)
	if err != nil {
		return fmt.Errorf("failed to parse URL %q. err: %v", *flagUri, err)
	}

	reqHeader := http.Header{}
	for _, h := range flagRequestHeader {
		chunks := strings.SplitN(h, ":", 2)
		reqHeader.Add(strings.TrimSpace(chunks[0]), strings.TrimSpace(chunks[1]))
	}

	resHeader := http.Header{}
	for _, h := range flagResponseHeader {
		chunks := strings.SplitN(h, ":", 2)
		resHeader.Add(strings.TrimSpace(chunks[0]), strings.TrimSpace(chunks[1]))
	}
	if resHeader.Get("content-type") == "" {
		resHeader.Add("content-type", "text/html; charset=utf-8")
	}
	e, err := signedexchange.NewExchange(parsedUrl, reqHeader, *flagResponseStatus, resHeader, payload)
	if err != nil {
		return err
	}
	if err := e.MiEncodePayload(*flagMIRecordSize, ver); err != nil {
		return err
	}

	var date time.Time
	if *flagDate == "" {
		date = time.Now()
	} else {
		var err error
		date, err = time.Parse(time.RFC3339, *flagDate)
		if err != nil {
			return err
		}
	}

	s := &signedexchange.Signer{
		Date:        date,
		Expires:     date.Add(*flagExpire),
		Certs:       certs,
		CertUrl:     certUrl,
		ValidityUrl: validityUrl,
		PrivKey:     privkey,
	}
	if err := e.AddSignatureHeader(s, ver); err != nil {
		return err
	}

	if fMsg != nil {
		if err := e.DumpSignedMessage(fMsg, s, ver); err != nil {
			return fmt.Errorf("failed to write signature message dump. err: %v", err)
		}
	}
	if fHdr != nil {
		if err := e.DumpExchangeHeaders(fHdr, ver); err != nil {
			return fmt.Errorf("failed to write headers cbor dump. err: %v", err)
		}
	}
	if err := e.Write(f, ver); err != nil {
		return fmt.Errorf("failed to write exchange. err: %v", err)
	}
	return nil
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
