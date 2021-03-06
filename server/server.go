package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hlandau/xlog"
	"github.com/miekg/dns"

	"github.com/namecoin/crosssign"
	"github.com/namecoin/qlib"
	"github.com/namecoin/safetlsa"
)

var log, logPublic = xlog.New("ncdns.server")

var Log = logPublic

type cachedCert struct {
	expiration time.Time
	certPem    string
}

type Server struct {
	cfg Config

	rootCert          []byte
	rootPriv          interface{}
	rootCertPem       []byte
	rootCertPemString string
	rootPrivPem       []byte
	tldCert           []byte
	tldPriv           interface{}
	tldCertPem        []byte
	tldCertPemString  string

	// These caches don't yet support stream isolation; see
	// https://github.com/namecoin/encaya/issues/8
	domainCertCache        map[string][]cachedCert
	domainCertCacheMutex   sync.RWMutex
	negativeCertCache      map[string][]cachedCert
	negativeCertCacheMutex sync.RWMutex
	originalCertCache      map[string][]cachedCert
	originalCertCacheMutex sync.RWMutex
}

//nolint:lll
type Config struct {
	DNSAddress string `default:"" usage:"Use this DNS server for DNS lookups.  (If left empty, the system resolver will be used.)"`
	DNSPort    int    `default:"53" usage:"Use this port for DNS lookups."`
	ListenIP   string `default:"127.127.127.127" usage:"Listen on this IP address."`

	RootCert    string `default:"root_cert.pem" usage:"Sign with this root CA certificate."`
	RootKey     string `default:"root_key.pem" usage:"Sign with this root CA private key."`
	ListenChain string `default:"listen_chain.pem" usage:"Listen with this TLS certificate chain."`
	ListenKey   string `default:"listen_key.pem" usage:"Listen with this TLS private key."`

	ConfigDir string // path to interpret filenames relative to
}

func (cfg *Config) cpath(s string) string {
	return filepath.Join(cfg.ConfigDir, s)
}

func (cfg *Config) processPaths() {
	cfg.RootCert = cfg.cpath(cfg.RootCert)
	cfg.RootKey = cfg.cpath(cfg.RootKey)
	cfg.ListenChain = cfg.cpath(cfg.ListenChain)
	cfg.ListenKey = cfg.cpath(cfg.ListenKey)
}

func New(cfg *Config) (s *Server, err error) {
	s = &Server{
		cfg: *cfg,
	}

	s.cfg.processPaths()

	s.rootCertPem, err = ioutil.ReadFile(s.cfg.RootCert)
	if err != nil {
		log.Fatalef(err, "Unable to read %s", s.cfg.RootCert)
	}

	s.rootCertPemString = string(s.rootCertPem)

	rootCertBlock, _ := pem.Decode(s.rootCertPem)
	//nolint:staticcheck // SA5011 Unreachable if nil due to log.Fatal
	if rootCertBlock == nil {
		log.Fatalef(err, "Unable to decode %s", s.cfg.RootCert)
	}

	//nolint:staticcheck // SA5011 Unreachable if nil due to log.Fatal
	s.rootCert = rootCertBlock.Bytes

	s.rootPrivPem, err = ioutil.ReadFile(s.cfg.RootKey)
	if err != nil {
		log.Fatalef(err, "Unable to read %s", s.cfg.RootKey)
	}

	rootPrivBlock, _ := pem.Decode(s.rootPrivPem)
	//nolint:staticcheck // SA5011 Unreachable if nil due to log.Fatal
	if rootPrivBlock == nil {
		log.Fatalef(err, "Unable to decode %s", s.cfg.RootKey)
	}

	//nolint:staticcheck // SA5011 Unreachable if nil due to log.Fatal
	rootPrivBytes := rootPrivBlock.Bytes

	s.rootPriv, err = x509.ParsePKCS8PrivateKey(rootPrivBytes)
	if err != nil {
		log.Fatalef(err, "Unable to parse %s", s.cfg.RootKey)
	}

	s.tldCert, s.tldPriv, err = safetlsa.GenerateTLDCA("bit", s.rootCert, s.rootPriv)
	if err != nil {
		log.Fatale(err, "Couldn't generate TLD CA")
	}

	s.tldCertPem = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: s.tldCert,
	})
	s.tldCertPemString = string(s.tldCertPem)

	s.domainCertCache = map[string][]cachedCert{}
	s.negativeCertCache = map[string][]cachedCert{}
	s.originalCertCache = map[string][]cachedCert{}

	http.HandleFunc("/lookup", s.lookupHandler)
	http.HandleFunc("/aia", s.aiaHandler)
	http.HandleFunc("/get-new-negative-ca", s.getNewNegativeCAHandler)
	http.HandleFunc("/cross-sign-ca", s.crossSignCAHandler)
	http.HandleFunc("/original-from-serial", s.originalFromSerialHandler)

	return s, nil
}

func (s *Server) Start() error {
	go s.doRunListenerTCP()
	go s.doRunListenerTLS()

	log.Info("Listeners started")

	return nil
}

func (s *Server) Stop() error {
	// Currently this doesn't actually stop the listeners, see
	// https://github.com/namecoin/encaya/issues/14
	return nil
}

func (s *Server) doRunListenerTCP() {
	err := http.ListenAndServe(s.cfg.ListenIP+":80", nil)
	log.Fatale(err)
}

func (s *Server) doRunListenerTLS() {
	err := http.ListenAndServeTLS(s.cfg.ListenIP+":443",
		s.cfg.ListenChain, s.cfg.ListenKey, nil)
	log.Fatale(err)
}

func (s *Server) getCachedDomainCerts(commonName string) (string, bool) {
	needRefresh := true
	results := ""

	s.domainCertCacheMutex.RLock()
	for _, cert := range s.domainCertCache[commonName] {
		if time.Until(cert.expiration) > 1*time.Minute {
			needRefresh = false
		}

		results = results + cert.certPem + "\n\n"
	}
	s.domainCertCacheMutex.RUnlock()

	return results, needRefresh
}

func (s *Server) cacheDomainCert(commonName, certPem string) {
	cert := cachedCert{
		expiration: time.Now().Add(2 * time.Minute),
		certPem:    certPem,
	}

	s.domainCertCacheMutex.Lock()
	if s.domainCertCache[commonName] == nil {
		s.domainCertCache[commonName] = []cachedCert{cert}
	} else {
		s.domainCertCache[commonName] = append(s.domainCertCache[commonName], cert)
	}
	s.domainCertCacheMutex.Unlock()
}

func (s *Server) popCachedDomainCertLater(commonName string) {
	time.Sleep(2 * time.Minute)

	s.domainCertCacheMutex.Lock()
	if s.domainCertCache[commonName] != nil {
		if len(s.domainCertCache[commonName]) > 1 {
			s.domainCertCache[commonName] = s.domainCertCache[commonName][1:]
		} else {
			delete(s.domainCertCache, commonName)
		}
	}
	s.domainCertCacheMutex.Unlock()
}

func (s *Server) getCachedNegativeCerts(commonName string) (string, bool) {
	needRefresh := true
	results := ""

	s.negativeCertCacheMutex.RLock()
	for _, cert := range s.negativeCertCache[commonName] {
		// Negative certs don't expire
		needRefresh = false

		results = results + cert.certPem + "\n\n"

		// We only need 1 negative cert
		break
	}
	s.negativeCertCacheMutex.RUnlock()

	return results, needRefresh
}

func (s *Server) cacheNegativeCert(commonName, certPem string) {
	cert := cachedCert{
		expiration: time.Now().Add(2 * time.Minute),
		certPem:    certPem,
	}

	s.negativeCertCacheMutex.Lock()
	if s.negativeCertCache[commonName] == nil {
		s.negativeCertCache[commonName] = []cachedCert{cert}
	} else {
		s.negativeCertCache[commonName] = append(s.negativeCertCache[commonName], cert)
	}
	s.negativeCertCacheMutex.Unlock()
}

func (s *Server) getCachedOriginalFromSerial(serial string) (string, bool) {
	needRefresh := true
	results := ""

	s.originalCertCacheMutex.RLock()
	for _, cert := range s.originalCertCache[serial] {
		// Original certs don't expire
		needRefresh = false

		results = results + cert.certPem + "\n\n"

		// We only need 1 original cert
		break
	}
	s.originalCertCacheMutex.RUnlock()

	return results, needRefresh
}

func (s *Server) cacheOriginalFromSerial(serial, certPem string) {
	cert := cachedCert{
		expiration: time.Now().Add(2 * time.Minute),
		certPem:    certPem,
	}

	s.originalCertCacheMutex.Lock()
	if s.originalCertCache[serial] == nil {
		s.originalCertCache[serial] = []cachedCert{cert}
	} else {
		s.originalCertCache[serial] = append(s.originalCertCache[serial], cert)
	}
	s.originalCertCacheMutex.Unlock()
}

func (s *Server) lookupHandler(w http.ResponseWriter, req *http.Request) {
	var err error

	domain := req.FormValue("domain")

	if domain == "Namecoin Root CA" {
		_, err = io.WriteString(w, s.rootCertPemString)
		if err != nil {
			log.Debuge(err, "write error")
		}

		return
	}

	if domain == ".bit TLD CA" {
		_, err = io.WriteString(w, s.tldCertPemString)
		if err != nil {
			log.Debuge(err, "write error")
		}

		return
	}

	cacheResults, needRefresh := s.getCachedDomainCerts(domain)
	if !needRefresh {
		_, err = io.WriteString(w, cacheResults)
		if err != nil {
			log.Debuge(err, "write error")
		}

		return
	}

	domain = strings.TrimSuffix(domain, " Domain CA")

	if strings.Contains(domain, " ") {
		// CommonNames that contain a space are usually CA's.  We
		// already stripped the suffixes of Namecoin-formatted CA's, so
		// if a space remains, just return.
		return
	}

	qparams := qlib.DefaultParams()
	qparams.Port = s.cfg.DNSPort
	qparams.Ad = true
	qparams.Fallback = true
	qparams.Tcp = true // Workaround for https://github.com/miekg/exdns/issues/19

	args := []string{}
	// Set the custom DNS server if requested
	if s.cfg.DNSAddress != "" {
		args = append(args, "@"+s.cfg.DNSAddress)
	}
	// Set qtype to TLSA
	args = append(args, "TLSA")
	// Set qname to all protocols and all ports of requested hostname
	args = append(args, "*."+domain)

	result, err := qparams.Do(args)
	if err != nil {
		// A DNS error occurred.
		log.Debuge(err, "qlib error")
		w.WriteHeader(500)

		return
	}

	if result.ResponseMsg == nil {
		// A DNS error occurred (nil response).
		w.WriteHeader(500)

		return
	}

	dnsResponse := result.ResponseMsg
	if dnsResponse.MsgHdr.Rcode != dns.RcodeSuccess && dnsResponse.MsgHdr.Rcode != dns.RcodeNameError {
		// A DNS error occurred (return code wasn't Success or NXDOMAIN).
		w.WriteHeader(500)

		return
	}

	if dnsResponse.MsgHdr.Rcode == dns.RcodeNameError {
		// Wildcard subdomain doesn't exist.
		// That means the domain doesn't use Namecoin-form DANE.
		// Return an empty cert list
		return
	}

	if !dnsResponse.MsgHdr.AuthenticatedData && !dnsResponse.MsgHdr.Authoritative {
		// For security reasons, we only trust records that are
		// authenticated (e.g. server is Unbound and has verified
		// DNSSEC sigs) or authoritative (e.g. server is ncdns and is
		// the owner of the requested zone).  If neither is the case,
		// then return an empty cert list.
		return
	}

	for _, rr := range dnsResponse.Answer {
		tlsa, ok := rr.(*dns.TLSA)
		if !ok {
			// Record isn't a TLSA record
			continue
		}

		safeCert, err := safetlsa.GetCertFromTLSA(domain, tlsa, s.tldCert, s.tldPriv)
		if err != nil {
			continue
		}

		safeCertPemBytes := pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: safeCert,
		})

		safeCertPem := string(safeCertPemBytes)

		_, err = io.WriteString(w, cacheResults+"\n\n"+safeCertPem)
		if err != nil {
			log.Debuge(err, "write error")
		}

		go s.cacheDomainCert(domain, safeCertPem)
		go s.popCachedDomainCertLater(domain)
	}
}

func (s *Server) aiaHandler(w http.ResponseWriter, req *http.Request) {
	var err error

	w.Header().Set("Content-Type", "application/pkix-cert")

	domain := req.FormValue("domain")

	if domain == "Namecoin Root CA" {
		_, err = io.WriteString(w, string(s.rootCert))
		if err != nil {
			log.Debuge(err, "write error")
		}

		return
	}

	if domain == ".bit TLD CA" {
		_, err = io.WriteString(w, string(s.tldCert))
		if err != nil {
			log.Debuge(err, "write error")
		}

		return
	}

	domain = strings.TrimSuffix(domain, " Domain AIA Parent CA")

	if strings.Contains(domain, " ") {
		// CommonNames that contain a space are usually CA's.  We
		// already stripped the suffixes of Namecoin-formatted CA's, so
		// if a space remains, just return.
		w.WriteHeader(404)

		return
	}

	qparams := qlib.DefaultParams()
	qparams.Port = s.cfg.DNSPort
	qparams.Ad = true
	qparams.Fallback = true
	qparams.Tcp = true // Workaround for https://github.com/miekg/exdns/issues/19

	args := []string{}
	// Set the custom DNS server if requested
	if s.cfg.DNSAddress != "" {
		args = append(args, "@"+s.cfg.DNSAddress)
	}
	// Set qtype to TLSA
	args = append(args, "TLSA")
	// Set qname to all protocols and all ports of requested hostname
	args = append(args, "*."+domain)

	result, err := qparams.Do(args)
	if err != nil {
		// A DNS error occurred.
		log.Debuge(err, "qlib error")
		w.WriteHeader(500)

		return
	}

	if result.ResponseMsg == nil {
		// A DNS error occurred (nil response).
		w.WriteHeader(500)

		return
	}

	dnsResponse := result.ResponseMsg
	if dnsResponse.MsgHdr.Rcode != dns.RcodeSuccess && dnsResponse.MsgHdr.Rcode != dns.RcodeNameError {
		// A DNS error occurred (return code wasn't Success or NXDOMAIN).
		w.WriteHeader(500)

		return
	}

	if dnsResponse.MsgHdr.Rcode == dns.RcodeNameError {
		// Wildcard subdomain doesn't exist.
		// That means the domain doesn't use Namecoin-form DANE.
		// Return an empty cert list
		w.WriteHeader(404)

		return
	}

	if !dnsResponse.MsgHdr.AuthenticatedData && !dnsResponse.MsgHdr.Authoritative {
		// For security reasons, we only trust records that are
		// authenticated (e.g. server is Unbound and has verified
		// DNSSEC sigs) or authoritative (e.g. server is ncdns and is
		// the owner of the requested zone).  If neither is the case,
		// then return an empty cert list.
		w.WriteHeader(404)

		return
	}

	pubSHA256Hex := req.FormValue("pubsha256")

	pubSHA256, err := hex.DecodeString(pubSHA256Hex)
	if err != nil {
		// Requested public key hash is malformed.
		w.WriteHeader(404)

		return
	}

	for _, rr := range dnsResponse.Answer {
		tlsa, ok := rr.(*dns.TLSA)
		if !ok {
			// Record isn't a TLSA record
			continue
		}

		// CA not in user's trust store; public key; not hashed
		if tlsa.Usage == 2 && tlsa.Selector == 1 && tlsa.MatchingType == 0 {
			tlsaPubBytes, err := hex.DecodeString(tlsa.Certificate)
			if err != nil {
				// TLSA record is malformed
				continue
			}

			tlsaPubSHA256 := sha256.Sum256(tlsaPubBytes)
			if !bytes.Equal(pubSHA256, tlsaPubSHA256[:]) {
				// TLSA record doesn't match requested public key hash
				continue
			}
		} else {
			// TLSA record isn't in the Namecoin CA form
			continue
		}

		safeCert, err := safetlsa.GetCertFromTLSA(domain, tlsa, s.tldCert, s.tldPriv)
		if err != nil {
			continue
		}

		_, err = io.WriteString(w, string(safeCert))
		if err != nil {
			log.Debuge(err, "write error")
		}

		break
	}
}

func (s *Server) getNewNegativeCAHandler(w http.ResponseWriter, req *http.Request) {
	restrictCert, restrictPriv, err := safetlsa.GenerateTLDExclusionCA("bit", s.rootCert, s.rootPriv)
	if err != nil {
		log.Debuge(err, "Error generating TLD exclusion CA")
	}

	restrictCertPem := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: restrictCert,
	})
	restrictCertPemString := string(restrictCertPem)

	restrictPrivBytes, err := x509.MarshalECPrivateKey(restrictPriv.(*ecdsa.PrivateKey))
	if err != nil {
		log.Debuge(err, "Unable to marshal ECDSA private key")
	}

	restrictPrivPem := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: restrictPrivBytes,
	})
	restrictPrivPemString := string(restrictPrivPem)

	_, err = io.WriteString(w, restrictCertPemString)
	if err != nil {
		log.Debuge(err, "write error")
	}

	_, err = io.WriteString(w, "\n\n")
	if err != nil {
		log.Debuge(err, "write error")
	}

	_, err = io.WriteString(w, restrictPrivPemString)
	if err != nil {
		log.Debuge(err, "write error")
	}
}

func (s *Server) crossSignCAHandler(w http.ResponseWriter, req *http.Request) {
	var err error

	toSignPEM := req.FormValue("to-sign")
	signerCertPEM := req.FormValue("signer-cert")
	signerKeyPEM := req.FormValue("signer-key")

	cacheKeyArray := sha256.Sum256([]byte(toSignPEM + "\n\n" + signerCertPEM + "\n\n" + signerKeyPEM + "\n\n"))
	cacheKey := hex.EncodeToString(cacheKeyArray[:])

	cacheResults, needRefresh := s.getCachedNegativeCerts(cacheKey)
	if !needRefresh {
		_, err = io.WriteString(w, cacheResults)
		if err != nil {
			log.Debuge(err, "write error")
		}

		return
	}

	toSignBlock, _ := pem.Decode([]byte(toSignPEM))
	signerCertBlock, _ := pem.Decode([]byte(signerCertPEM))
	signerKeyBlock, _ := pem.Decode([]byte(signerKeyPEM))

	signerKey, err := x509.ParseECPrivateKey(signerKeyBlock.Bytes)
	if err != nil {
		log.Debuge(err, "Unable to parse ECDSA private key")

		return
	}

	resultBytes, err := crosssign.CrossSign(toSignBlock.Bytes, signerCertBlock.Bytes, signerKey)
	if err != nil {
		log.Debuge(err, "Unable to cross-sign")

		return
	}

	resultPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: resultBytes,
	})
	resultPEMString := string(resultPEM)

	resultParsed, err := x509.ParseCertificate(resultBytes)
	if err != nil {
		log.Debuge(err, "Unable to extract serial number from cross-signed CA")
	}

	_, err = io.WriteString(w, resultPEMString)
	if err != nil {
		log.Debuge(err, "write error")
	}

	s.cacheNegativeCert(cacheKey, resultPEMString)
	s.cacheOriginalFromSerial(resultParsed.SerialNumber.String(), toSignPEM)
}

func (s *Server) originalFromSerialHandler(w http.ResponseWriter, req *http.Request) {
	serial := req.FormValue("serial")

	cacheResults, needRefresh := s.getCachedOriginalFromSerial(serial)
	if !needRefresh {
		_, err := io.WriteString(w, cacheResults)
		if err != nil {
			log.Debuge(err, "write error")
		}
	}
}

func GenerateCerts(cfg *Config) {
	var (
		err                 error
		listenCertPem       []byte
		listenCertPemString string
	)

	s := &Server{
		cfg: *cfg,
	}

	s.cfg.processPaths()

	s.rootCert, s.rootPriv, err = safetlsa.GenerateRootCA("Namecoin")
	if err != nil {
		log.Fatale(err, "Couldn't generate root CA")
	}

	rootPrivBytes, err := x509.MarshalPKCS8PrivateKey(s.rootPriv)
	if err != nil {
		log.Fatale(err, "Unable to marshal private key")
	}

	s.rootCertPem = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: s.rootCert,
	})
	s.rootCertPemString = string(s.rootCertPem)

	s.rootPrivPem = pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: rootPrivBytes,
	})

	s.tldCert, s.tldPriv, err = safetlsa.GenerateTLDCA("bit", s.rootCert, s.rootPriv)
	if err != nil {
		log.Fatale(err, "Couldn't generate TLD CA")
	}

	s.tldCertPem = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: s.tldCert,
	})
	s.tldCertPemString = string(s.tldCertPem)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)

	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Fatale(err, "Unable to generate serial number")
	}

	listenPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatale(err, "Unable to generate listening key")
	}

	listenPrivBytes, err := x509.MarshalPKCS8PrivateKey(listenPriv)
	if err != nil {
		log.Fatale(err, "Unable to marshal private key")
	}

	listenTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "aia.x--nmc.bit",
			SerialNumber: "Namecoin TLS Certificate",
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(43800 * time.Hour),

		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,

		DNSNames: []string{"aia.x--nmc.bit"},
	}

	tldCertParsed, err := x509.ParseCertificate(s.tldCert)
	if err != nil {
		log.Fatale(err, "Unable to parse TLD cert")
	}

	listenCert, err := x509.CreateCertificate(rand.Reader, &listenTemplate,
		tldCertParsed, &listenPriv.PublicKey, s.tldPriv)
	if err != nil {
		log.Fatale(err, "Unable to create listening cert")
	}

	listenCertPem = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: listenCert,
	})
	listenCertPemString = string(listenCertPem)

	listenPrivPem := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: listenPrivBytes,
	})

	err = ioutil.WriteFile(s.cfg.RootCert, s.rootCertPem, 0600)
	if err != nil {
		log.Fatalef(err, "Unable to write %s", s.cfg.RootCert)
	}

	err = ioutil.WriteFile(s.cfg.RootKey, s.rootPrivPem, 0600)
	if err != nil {
		log.Fatalef(err, "Unable to write %s", s.cfg.RootKey)
	}

	listenChainPemString := listenCertPemString + "\n\n" + s.tldCertPemString + "\n\n" + s.rootCertPemString
	listenChainPem := []byte(listenChainPemString)

	err = ioutil.WriteFile(s.cfg.ListenChain, listenChainPem, 0600)
	if err != nil {
		log.Fatalef(err, "Unable to write %s", s.cfg.ListenChain)
	}

	err = ioutil.WriteFile(s.cfg.ListenKey, listenPrivPem, 0600)
	if err != nil {
		log.Fatalef(err, "Unable to write %s", s.cfg.ListenKey)
	}
}
