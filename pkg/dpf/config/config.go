// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package config loads ordinary DPF site YAML without Host defaults or ambient access.
package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"k8s.io/apimachinery/pkg/util/validation"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/yamlinput"
	yaml "gopkg.in/yaml.v3"
)

type FileRef struct {
	File string `yaml:"file"`
}
type PasswordRef struct {
	Env  *string `yaml:"env,omitempty"`
	File *string `yaml:"file,omitempty"`
}
type Credential struct {
	Username    string      `yaml:"username"`
	PasswordRef PasswordRef `yaml:"passwordRef"`
}
type TLS struct {
	CAFile *string `yaml:"caFile,omitempty"`
}
type Endpoint struct {
	Name          *string `yaml:"name,omitempty"`
	BMCEndpoint   string  `yaml:"bmcEndpoint"`
	CredentialRef string  `yaml:"credentialRef"`
	SystemURI     *string `yaml:"systemURI,omitempty"`
	TLS           *TLS    `yaml:"tls,omitempty"`
}
type PullSecret struct {
	Name string `yaml:"name"`
}
type Probes struct {
	Mode             *string      `yaml:"mode,omitempty"`
	Namespace        *string      `yaml:"namespace,omitempty"`
	ImageRepository  *string      `yaml:"imageRepository,omitempty"`
	ImagePullSecrets []PullSecret `yaml:"imagePullSecrets,omitempty"`
}
type Limits struct {
	Timeout           *string `yaml:"timeout,omitempty"`
	RequestTimeout    *string `yaml:"requestTimeout,omitempty"`
	MaxConcurrentBMCs *int    `yaml:"maxConcurrentBMCs,omitempty"`
}
type Site struct {
	Management struct {
		KubeconfigRef FileRef `yaml:"kubeconfigRef"`
		Context       *string `yaml:"context,omitempty"`
	} `yaml:"management"`
	Credentials map[string]Credential `yaml:"credentials"`
	Discovery   struct {
		DPUs   []Endpoint `yaml:"dpus"`
		Hosts  []Endpoint `yaml:"hosts"`
		TLS    *TLS       `yaml:"tls,omitempty"`
		Probes Probes     `yaml:"probes,omitempty"`
		Limits Limits     `yaml:"limits,omitempty"`
	} `yaml:"discovery"`
	Inventory *inventory.Inventory `yaml:"inventory,omitempty"`
}
type Input struct {
	Site     Site
	Source   []byte
	Path     string
	Document *yaml.Node
}

// Access is private runtime state. It deliberately cannot be serialized or logged.
type Access struct {
	username string
	password string
	tls      *tls.Config
	ca       []byte
}

func (a Access) Credentials() (string, string) { return a.username, a.password }
func (a Access) TLSConfig() *tls.Config        { return a.tls.Clone() }
func (a Access) PublicCA() []byte              { return bytes.Clone(a.ca) }
func (a Access) String() string                { return "<private BMC access>" }

type Target struct {
	Endpoint  string
	SystemURI string
	InputRef  string
	Role      string
	Access    Access
}
type Request struct {
	Input             Input
	Targets           []Target
	Kubeconfig        string
	Context           string
	Mode              string
	Namespace         string
	ImageRepository   string
	ImagePullSecrets  []string
	Timeout           time.Duration
	RequestTimeout    time.Duration
	MaxConcurrentBMCs int
}

func regularFile(p string, limit int64) ([]byte, error) {
	before, statErr := os.Stat(p)
	if statErr != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, fmt.Errorf("reference must be a bounded regular file")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("cannot open referenced file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("reference must be a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, fmt.Errorf("cannot read bounded file")
	}
	return b, nil
}
func Load(p string) (Input, error) {
	abs, err := filepath.Abs(p)
	if err != nil || p == "" || p == "-" {
		return Input{}, fmt.Errorf("--user-config must name one local YAML file")
	}
	b, err := regularFile(abs, 16<<20)
	if err != nil {
		return Input{}, err
	}
	in, err := Decode(b, abs)
	if err != nil {
		return Input{}, fmt.Errorf("%s: %w", abs, err)
	}
	return in, nil
}
func Decode(b []byte, p string) (Input, error) {
	if len(b) > 16<<20 {
		return Input{}, fmt.Errorf("YAML input exceeds 16 MiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return Input{}, fmt.Errorf("invalid YAML")
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Input{}, fmt.Errorf("expected one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return Input{}, fmt.Errorf("expected mapping document")
	}
	if err := validateKeys(&doc, map[*yaml.Node]bool{}, map[*yaml.Node]bool{}, 0); err != nil {
		return Input{}, err
	}
	if err := validateScalarTypes(doc.Content[0], ""); err != nil {
		return Input{}, err
	}
	expanded, err := yamlinput.Expand(&doc)
	if err != nil {
		return Input{}, err
	}
	if err := validateScalarTypes(expanded.Content[0], ""); err != nil {
		return Input{}, err
	}
	encoded, err := yamlinput.Encode(expanded)
	if err != nil {
		return Input{}, err
	}
	var site Site
	strict := yaml.NewDecoder(bytes.NewReader(encoded))
	strict.KnownFields(true)
	if err := strict.Decode(&site); err != nil {
		return Input{}, fmt.Errorf("unknown field or invalid type in DPF configuration")
	}
	return Input{Site: site, Source: bytes.Clone(b), Path: p, Document: expanded}, nil
}
func validateKeys(n *yaml.Node, active, visited map[*yaml.Node]bool, depth int) error {
	if n == nil || depth > 64 || active[n] {
		return fmt.Errorf("cyclic or excessive YAML aliases")
	}
	if visited[n] {
		return nil
	}
	visited[n] = true
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		return validateKeys(n.Alias, active, visited, depth+1)
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if (k.Tag != "!!str" && k.Tag != "!!merge") || seen[k.Value] {
				return fmt.Errorf("duplicate or non-string key at line %d", k.Line)
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := validateKeys(c, active, visited, depth+1); err != nil {
			return err
		}
	}
	return nil
}
func Resolve(in Input) (Request, error) {
	r := Request{Input: in, Timeout: 10 * time.Minute, RequestTimeout: 15 * time.Second, MaxConcurrentBMCs: 4, Mode: "management"}
	s := in.Site
	base := filepath.Dir(in.Path)
	if s.Management.KubeconfigRef.File == "" {
		return r, fmt.Errorf("/management/kubeconfigRef/file is required")
	}
	r.Kubeconfig = resolvePath(base, s.Management.KubeconfigRef.File)
	if s.Management.Context != nil {
		if *s.Management.Context == "" {
			return r, fmt.Errorf("/management/context must not be empty")
		}
		r.Context = *s.Management.Context
	}
	p := s.Discovery.Probes
	if p.Mode != nil {
		r.Mode = *p.Mode
	}
	if r.Mode != "management" && r.Mode != "inventory" {
		return r, fmt.Errorf("/discovery/probes/mode must be management or inventory")
	}
	if p.Namespace != nil {
		r.Namespace = *p.Namespace
		if r.Namespace == "" || len(r.Namespace) > 63 || !dnsLabel(r.Namespace) {
			return r, fmt.Errorf("invalid /discovery/probes/namespace")
		}
	}
	if p.ImageRepository != nil {
		r.ImageRepository = *p.ImageRepository
		if r.ImageRepository == "" || strings.ContainsAny(r.ImageRepository, "@ \t\n") || strings.Contains(r.ImageRepository, "://") {
			return r, fmt.Errorf("invalid probe repository")
		}
		last := r.ImageRepository[strings.LastIndex(r.ImageRepository, "/")+1:]
		if strings.Contains(last, ":") {
			return r, fmt.Errorf("probe repository must not have a tag")
		}
	}
	for _, secret := range p.ImagePullSecrets {
		if p.Namespace == nil || secret.Name == "" || !dnsLabel(secret.Name) {
			return r, fmt.Errorf("imagePullSecrets requires an explicit namespace and valid names")
		}
		r.ImagePullSecrets = append(r.ImagePullSecrets, secret.Name)
	}
	l := s.Discovery.Limits
	for _, f := range []struct {
		value *string
		dest  *time.Duration
		max   time.Duration
	}{{l.Timeout, &r.Timeout, time.Hour}, {l.RequestTimeout, &r.RequestTimeout, 2 * time.Minute}} {
		if f.value != nil {
			d, err := time.ParseDuration(*f.value)
			if err != nil || d < time.Second || d > f.max {
				return r, fmt.Errorf("invalid /discovery/limits duration")
			}
			*f.dest = d
		}
	}
	if l.MaxConcurrentBMCs != nil {
		r.MaxConcurrentBMCs = *l.MaxConcurrentBMCs
	}
	if r.RequestTimeout > r.Timeout || r.MaxConcurrentBMCs < 1 || r.MaxConcurrentBMCs > 16 {
		return r, fmt.Errorf("invalid /discovery/limits")
	}
	if len(s.Discovery.DPUs) == 0 || len(s.Discovery.Hosts) == 0 || len(s.Discovery.DPUs)+len(s.Discovery.Hosts) > 256 {
		return r, fmt.Errorf("discovery requires 1-256 explicit DPU/host endpoints and both roles")
	}
	seen := map[string]bool{}
	accesses := map[string]Access{}
	for _, group := range []struct {
		role      string
		endpoints []Endpoint
	}{{"dpus", s.Discovery.DPUs}, {"hosts", s.Discovery.Hosts}} {
		for i, e := range group.endpoints {
			ptr := fmt.Sprintf("/discovery/%s/%d", group.role, i)
			origin, err := Origin(e.BMCEndpoint)
			if err != nil {
				return r, fmt.Errorf("%s/bmcEndpoint: %w", ptr, err)
			}
			if seen[origin] {
				return r, fmt.Errorf("duplicate BMC origin at %s", ptr)
			}
			seen[origin] = true
			selector := ""
			if e.SystemURI != nil {
				selector = *e.SystemURI
				if err := ResourceURI(selector); err != nil {
					return r, fmt.Errorf("%s/systemURI: invalid Redfish resource", ptr)
				}
			}
			c, ok := s.Credentials[e.CredentialRef]
			if !ok || c.Username == "" {
				return r, fmt.Errorf("%s/credentialRef must reference a username/passwordRef", ptr)
			}
			policy := s.Discovery.TLS
			if e.TLS != nil {
				policy = e.TLS
			}
			caPath := ""
			if policy != nil && policy.CAFile != nil {
				if *policy.CAFile == "" {
					return r, fmt.Errorf("empty caFile")
				}
				caPath = resolvePath(base, *policy.CAFile)
			}
			cacheKey := e.CredentialRef + "\x00" + caPath
			a, ok := accesses[cacheKey]
			if !ok {
				a, err = resolveAccess(c, caPath, base)
				if err != nil {
					return r, fmt.Errorf("%s: %w", ptr, err)
				}
				accesses[cacheKey] = a
			}
			r.Targets = append(r.Targets, Target{Endpoint: origin, SystemURI: selector, InputRef: ptr, Role: group.role, Access: a})
		}
	}
	return r, nil
}
func dnsLabel(s string) bool { return len(validation.IsDNS1123Label(s)) == 0 }
func resolvePath(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}
func resolveAccess(c Credential, caPath, base string) (Access, error) {
	p := c.PasswordRef
	if (p.Env == nil) == (p.File == nil) {
		return Access{}, fmt.Errorf("passwordRef requires exactly one env or file")
	}
	var password string
	if p.Env != nil {
		if *p.Env == "" {
			return Access{}, fmt.Errorf("empty passwordRef.env")
		}
		password = os.Getenv(*p.Env)
	} else {
		if *p.File == "" {
			return Access{}, fmt.Errorf("empty passwordRef.file")
		}
		b, err := regularFile(resolvePath(base, *p.File), 64<<10)
		if err != nil {
			return Access{}, fmt.Errorf("cannot resolve password file")
		}
		if !utf8.Valid(b) {
			return Access{}, fmt.Errorf("password file must be UTF-8")
		}
		password = string(b)
		if strings.HasSuffix(password, "\n") {
			password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
		}
	}
	if password == "" || len(password) > 64<<10 {
		return Access{}, fmt.Errorf("password reference is missing, empty or too large")
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	var ca []byte
	if caPath != "" {
		ca, err = regularFile(caPath, 1<<20)
		if err != nil || !roots.AppendCertsFromPEM(ca) {
			return Access{}, fmt.Errorf("invalid CA file")
		}
	}
	return Access{username: c.Username, password: password, tls: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, ca: ca}, nil
}
func Origin(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("expected HTTPS origin without userinfo/path/query/fragment")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid port")
		}
	}
	if port == "" || port == "443" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	} else {
		host = net.JoinHostPort(host, port)
	}
	return "https://" + host, nil
}
func ResourceURI(s string) error {
	u, e := url.Parse(s)
	if e != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/redfish/v1/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Path, "..") || path.Clean(u.Path) != u.Path || u.RawPath != "" {
		return fmt.Errorf("invalid resource URI")
	}
	return nil
}

// Saved patches only generated inventory and file references whose base changed.
func Saved(in Input, inv *inventory.Inventory, destination string) ([]byte, error) {
	doc, err := yamlinput.Expand(in.Document)
	if err != nil {
		return nil, err
	}
	var replacement yaml.Node
	if err := replacement.Encode(inv); err != nil {
		return nil, err
	}
	yamlinput.Set(doc.Content[0], "inventory", &replacement)
	if filepath.Dir(destination) != filepath.Dir(in.Path) {
		if err := rebase(doc.Content[0], filepath.Dir(in.Path), filepath.Dir(destination), ""); err != nil {
			return nil, err
		}
	}
	return yamlinput.Encode(doc)
}
func rebase(n *yaml.Node, oldBase, newBase, p string) error {
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i].Value
			value := n.Content[i+1]
			next := p + "/" + key
			if key == "inventory" && p == "" {
				continue
			}
			isFile := key == "caFile" || key == "file" && (strings.HasSuffix(p, "kubeconfigRef") || strings.HasSuffix(p, "passwordRef"))
			if isFile && value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Value != "" {
				abs := resolvePath(oldBase, value.Value)
				rel, err := filepath.Rel(newBase, abs)
				if err != nil {
					return err
				}
				value.Value = rel
			} else if err := rebase(value, oldBase, newBase, next); err != nil {
				return err
			}
		}
	} else if n.Kind == yaml.SequenceNode {
		for _, v := range n.Content {
			if err := rebase(v, oldBase, newBase, p); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateScalarTypes(n *yaml.Node, p string) error {
	if p == "/inventory" {
		return nil
	}
	if n.Kind == yaml.AliasNode {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil
		}
		expected := "!!str"
		if p == "/discovery/limits/maxConcurrentBMCs" {
			expected = "!!int"
		}
		if n.Tag != expected {
			return fmt.Errorf("%s has invalid scalar type at line %d", p, n.Line)
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			if err := validateScalarTypes(n.Content[i+1], p+"/"+n.Content[i].Value); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, v := range n.Content {
			if err := validateScalarTypes(v, fmt.Sprintf("%s/%d", p, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadReference rejects streams/devices and bounds local reference reads.
func ReadReference(filename string, limit int64) ([]byte, error) { return regularFile(filename, limit) }
