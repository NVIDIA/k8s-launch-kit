// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
)

func TestSnapshotRequestForValidationFreezesPublicAccessScope(t *testing.T) {
	ssh := &SSHAccess{username: "private-user", password: "private-password", privateKey: []byte("private-key"), knownHosts: []byte("trust"), Sudo: true}
	request := Request{Input: Input{Source: []byte("source")}, Targets: []Target{{Name: "host", Endpoint: "ssh://host.example", SSH: ssh}}, Switches: []SwitchTarget{{Name: "switch", Access: ssh, Ports: []SwitchPort{{Name: "swp1", DPUSerial: "SERIAL"}}}}, ImagePullSecrets: []string{"registry"}}
	frozen, err := SnapshotRequestForValidation(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Input.Source[0] = 'x'
	request.Targets[0].Endpoint = "ssh://changed.example"
	request.Targets[0].SSH.Sudo = false
	request.Switches[0].Ports[0].Name = "swp9"
	request.ImagePullSecrets[0] = "changed"
	if string(frozen.Input.Source) != "source" || frozen.Targets[0].Endpoint != "ssh://host.example" || !frozen.Targets[0].SSH.Sudo || frozen.Switches[0].Ports[0].Name != "swp1" || frozen.ImagePullSecrets[0] != "registry" {
		t.Fatal("observer retained caller-owned scope")
	}
	user, password := frozen.Targets[0].SSH.Credentials()
	if user != "private-user" || password != "private-password" || string(frozen.Targets[0].SSH.PrivateKey()) != "private-key" {
		t.Fatal("snapshot lost resolved private access")
	}
}
