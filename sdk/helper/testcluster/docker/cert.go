// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
)

// ReloadFunc are functions that are called when a reload is requested
type ReloadFunc func() error

// CertificateGetter satisfies ReloadFunc and its GetCertificate method
// satisfies the tls.GetCertificate function signature.  Currently it does not
// allow changing paths after the fact.
type CertificateGetter struct {
	sync.RWMutex

	cert *tls.Certificate

	certFile   string
	keyFile    string
	passphrase string
}

func NewCertificateGetter(certFile, keyFile, passphrase string) *CertificateGetter {
	return &CertificateGetter{
		certFile:   certFile,
		keyFile:    keyFile,
		passphrase: passphrase,
	}
}

func (cg *CertificateGetter) Reload() error {
	certPEMBlock, err := os.ReadFile(cg.certFile)
	if err != nil {
		return err
	}
	keyPEMBlock, err := os.ReadFile(cg.keyFile)
	if err != nil {
		return err
	}

	// Check for encrypted pem block
	keyBlock, _ := pem.Decode(keyPEMBlock)
	if keyBlock == nil {
		return errors.New("decoded PEM is blank")
	}

	if x509.IsEncryptedPEMBlock(keyBlock) {
		keyBlock.Bytes, err = x509.DecryptPEMBlock(keyBlock, []byte(cg.passphrase))
		if err != nil {
			return fmt.Errorf("Decrypting PEM block failed %w", err)
		}
		keyPEMBlock = pem.EncodeToMemory(keyBlock)
	}

	cert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
	if err != nil {
		return err
	}

	cg.Lock()
	defer cg.Unlock()

	cg.cert = &cert

	return nil
}

func (cg *CertificateGetter) GetCertificate(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cg.RLock()
	defer cg.RUnlock()

	if cg.cert == nil {
		return nil, errors.New("nil certificate")
	}

	return cg.cert, nil
}
