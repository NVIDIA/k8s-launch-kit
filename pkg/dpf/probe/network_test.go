// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package probe

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("host-network facts", func() {
	It("validates and serializes one canonical IPv4 VIP request", func() {
		req := networkRequest("192.0.2.20")
		files, err := requestFiles(req)
		Expect(err).NotTo(HaveOccurred())
		var decoded struct {
			TenantVIP string `json:"tenantVIP"`
		}
		Expect(json.Unmarshal([]byte(files["request.json"]), &decoded)).To(Succeed())
		Expect(decoded.TenantVIP).To(Equal("192.0.2.20"))
		files, err = requestFiles(networkRequest(""))
		Expect(err).NotTo(HaveOccurred())
		Expect(files["request.json"]).NotTo(ContainSubstring("tenantVIP"))

		for _, vip := range []string{"192.0.2.020", "::ffff:192.0.2.20", "2001:db8::20", "127.0.0.1", "169.254.1.1", "224.0.0.1", "not-an-ip"} {
			bad := networkRequest(vip)
			_, err := requestFiles(bad)
			Expect(err).To(HaveOccurred(), vip)
		}
	})

	It("accepts complete node-network facts and an explicit failed route lookup", func() {
		req := networkRequest("192.0.2.20")
		out := validNetworkResponse()
		Expect(ValidateNetworkResponse(out, req, "node-network")).To(Succeed())
		adminDown := false
		out.Interfaces[0].AdminUp = &adminDown
		wire, err := json.Marshal(out)
		Expect(err).NotTo(HaveOccurred())
		var decoded Response
		Expect(json.Unmarshal(wire, &decoded)).To(Succeed())
		Expect(decoded.Interfaces[0].AdminUp).NotTo(BeNil())
		Expect(*decoded.Interfaces[0].AdminUp).To(BeFalse(), "explicit down must remain distinct from unknown")
		Expect(ValidateNetworkResponse(decoded, req, "node-network")).To(Succeed())

		out.TenantVIPRoute = &inventory.RouteLookup{
			Destination: "192.0.2.20",
			ReasonCode:  "ROUTE_LOOKUP_FAILED",
			Message:     "IPv4 host route lookup failed",
		}
		Expect(ValidateNetworkResponse(out, req, "node-network")).To(Succeed())
	})

	It("requires network facts only for node-network and requires a route result when requested", func() {
		req := networkRequest("192.0.2.20")
		out := Response{NetworkMode: "pod-network"}
		Expect(ValidateNetworkResponse(out, req, "pod-network")).To(Succeed())
		out.Interfaces = []inventory.Interface{}
		out.Routes = []inventory.Route{}
		Expect(ValidateNetworkResponse(out, req, "pod-network")).To(Succeed(), "the fixed collector emits empty host-fact arrays for pod-network mode")
		out.Interfaces = []inventory.Interface{{Name: "unexpected"}}
		Expect(ValidateNetworkResponse(out, req, "pod-network")).NotTo(Succeed())

		out = validNetworkResponse()
		out.TenantVIPRoute = nil
		Expect(ValidateNetworkResponse(out, req, "node-network")).NotTo(Succeed())
		out = validNetworkResponse()
		Expect(ValidateNetworkResponse(out, networkRequest(""), "node-network")).NotTo(Succeed())
		out.TenantVIPRoute = nil
		Expect(ValidateNetworkResponse(out, networkRequest(""), "node-network")).To(Succeed())
	})

	It("rejects malformed or contradictory interfaces and route tables", func() {
		cases := []struct {
			name   string
			mutate func(*Response)
		}{
			{name: "missing interfaces", mutate: func(out *Response) { out.Interfaces = nil }},
			{name: "missing routes", mutate: func(out *Response) { out.Routes = nil }},
			{name: "missing administrative state", mutate: func(out *Response) { out.Interfaces[0].AdminUp = nil }},
			{name: "missing operational state", mutate: func(out *Response) { out.Interfaces[0].OperState = "" }},
			{name: "duplicate interface name", mutate: func(out *Response) {
				out.Interfaces = append(out.Interfaces, out.Interfaces[0])
				out.Interfaces[1].Index++
			}},
			{name: "duplicate interface index", mutate: func(out *Response) {
				other := out.Interfaces[0]
				other.Name = "eth9"
				out.Interfaces = append(out.Interfaces, other)
			}},
			{name: "invalid interface index", mutate: func(out *Response) { out.Interfaces[0].Index = 0 }},
			{name: "invalid prefix", mutate: func(out *Response) { out.Interfaces[0].Addresses = []string{"192.0.2.1/99"} }},
			{name: "mapped prefix", mutate: func(out *Response) { out.Interfaces[0].Addresses = []string{"::ffff:192.0.2.1/120"} }},
			{name: "duplicate prefix", mutate: func(out *Response) { out.Interfaces[0].Addresses = []string{"192.0.2.1/24", "192.0.2.1/24"} }},
			{name: "route to unknown index", mutate: func(out *Response) {
				out.Routes = []inventory.Route{{Destination: "default", InterfaceIndex: 77, Table: 254}}
			}},
			{name: "invalid route destination", mutate: func(out *Response) { out.Routes = []inventory.Route{{Destination: "nonsense", Table: 254}} }},
			{name: "invalid route gateway", mutate: func(out *Response) {
				out.Routes = []inventory.Route{{Destination: "default", Gateway: "bad", InterfaceIndex: 2, Table: 254}}
			}},
		}
		for _, tc := range cases {
			out := validNetworkResponse()
			tc.mutate(&out)
			Expect(ValidateNetworkResponse(out, networkRequest("192.0.2.20"), "node-network")).NotTo(Succeed(), tc.name)
		}
	})

	It("rejects missing, incomplete, mismatched, or unsafe route lookup facts", func() {
		cases := []struct {
			name   string
			lookup *inventory.RouteLookup
		}{
			{name: "wrong destination", lookup: &inventory.RouteLookup{Destination: "192.0.2.21", InterfaceIndex: 2, Source: "192.0.2.2"}},
			{name: "missing source", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", InterfaceIndex: 2}},
			{name: "invalid source", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", InterfaceIndex: 2, Source: "bad"}},
			{name: "wrong-family source", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", InterfaceIndex: 2, Source: "2001:db8::2"}},
			{name: "unknown interface", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", InterfaceIndex: 9, Source: "192.0.2.2"}},
			{name: "reason without message", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", ReasonCode: "ROUTE_LOOKUP_FAILED"}},
			{name: "message without reason", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", Message: "failed"}},
			{name: "failure with partial success data", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", ReasonCode: "ROUTE_LOOKUP_FAILED", Message: "failed", InterfaceIndex: 2}},
			{name: "unsafe reason", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", ReasonCode: "failed\nsecret", Message: "failed"}},
			{name: "unsafe message", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", ReasonCode: "ROUTE_LOOKUP_FAILED", Message: strings.Repeat("x", maxRouteFailureMessage+1)}},
			{name: "unrecognized failure message", lookup: &inventory.RouteLookup{Destination: "192.0.2.20", ReasonCode: "ROUTE_LOOKUP_FAILED", Message: "raw command stderr"}},
		}
		for _, tc := range cases {
			out := validNetworkResponse()
			out.TenantVIPRoute = tc.lookup
			Expect(ValidateNetworkResponse(out, networkRequest("192.0.2.20"), "node-network")).NotTo(Succeed(), tc.name)
		}
	})

	It("keeps transient Kubernetes labels out of saved node inventory", func() {
		value, err := json.Marshal(inventory.Node{Name: "node", Labels: map[string]string{"secret.example/key": "value"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(value)).NotTo(ContainSubstring("secret.example/key"))
		Expect(string(value)).NotTo(ContainSubstring("labels"))
	})
})

func networkRequest(vip string) Request {
	return Request{
		TenantVIP:      vip,
		Version:        ProtocolVersion,
		RunID:          "network-test",
		Deadline:       time.Now().Add(time.Minute),
		RequestTimeout: time.Second,
		Destinations:   []Destination{{ID: "management", Endpoint: "https://api.example.invalid", Path: "/version"}},
	}
}

func validNetworkResponse() Response {
	adminUp := true
	return Response{
		NetworkMode: "node-network",
		Interfaces: []inventory.Interface{{
			Name:      "bond0",
			Index:     2,
			MTU:       1500,
			AdminUp:   &adminUp,
			OperState: "UP",
			Addresses: []string{"192.0.2.2/24", "2001:db8::2/64"},
		}},
		Routes:         []inventory.Route{{Destination: "default", Gateway: "192.0.2.1", InterfaceIndex: 2, Table: 254}},
		TenantVIPRoute: &inventory.RouteLookup{Destination: "192.0.2.20", InterfaceIndex: 2, Source: "192.0.2.2", Gateway: "192.0.2.1"},
	}
}
