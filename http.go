package main

import (
	"crypto/tls"
	"net/http"
)

type HTTPClientConfig struct {
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}

func newHTTPClient(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if cfg.HTTP.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec
		}
	}

	return &http.Client{
		Transport: transport,
	}
}
