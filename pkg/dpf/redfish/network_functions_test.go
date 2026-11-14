// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type networkGraphFixture struct {
	docs     map[string]Document
	reads    map[string]int
	statuses map[string]int
	methods  []string
	mutate   func(string, int, Document)
}

func (f *networkGraphFixture) handler(w http.ResponseWriter, r *http.Request) {
	uri := strings.TrimSuffix(r.URL.Path, "/")
	f.methods = append(f.methods, r.Method)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if status := f.statuses[uri]; status != 0 {
		w.WriteHeader(status)
		return
	}
	doc, ok := f.docs[uri]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.reads[uri]++
	if f.mutate != nil {
		f.mutate(uri, f.reads[uri], doc)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func newNetworkGraphFixture() *networkGraphFixture {
	link := func(uri string) Document { return Document{"@odata.id": uri} }
	collection := func(uris ...string) Document {
		members := make([]any, 0, len(uris))
		for _, uri := range uris {
			members = append(members, link(uri))
		}
		return Document{"Members@odata.count": len(uris), "Members": members}
	}
	const base = "/redfish/v1/Chassis/selected/NetworkAdapters/card0"
	function := func(id, mac string) Document {
		return Document{"Id": id, "NetDevFuncType": "Ethernet", "Ethernet": Document{"PermanentMACAddress": mac},
			"AssignablePhysicalNetworkPorts": []any{link(base + "/Ports/Port_" + id)}}
	}
	return &networkGraphFixture{docs: map[string]Document{
		"/redfish/v1/Chassis/selected":                 {"NetworkAdapters": link("/redfish/v1/Chassis/selected/NetworkAdapters")},
		"/redfish/v1/Chassis/selected/NetworkAdapters": collection(base),
		base:                               {"Id": "card0", "NetworkDeviceFunctions": link(base + "/NetworkDeviceFunctions")},
		base + "/NetworkDeviceFunctions":   collection(base+"/NetworkDeviceFunctions/0", base+"/NetworkDeviceFunctions/1"),
		base + "/NetworkDeviceFunctions/0": function("0", "02:00:00:00:00:02"),
		base + "/NetworkDeviceFunctions/1": function("1", "02:00:00:00:00:01"),
	}, reads: map[string]int{}, statuses: map[string]int{}}
}

func networkGraphClient(f *networkGraphFixture) (*Client, func()) {
	server := httptest.NewTLSServer(http.HandlerFunc(f.handler))
	return &Client{origin: server.URL, http: server.Client()}, server.Close
}

var _ = Describe("identity-selected NetworkDeviceFunction MAC inventory", func() {
	It("returns normalized Ethernet permanent MACs without inferring assignment or ARM names", func() {
		fixture := newNetworkGraphFixture()
		fixture.docs["/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions/0"]["Ethernet"] = Document{"PermanentMACAddress": "F4:20:4D:16:19:B0"}
		client, closeServer := networkGraphClient(fixture)
		defer closeServer()
		card := Resource{URI: "/redfish/v1/Chassis/selected", Document: fixture.docs["/redfish/v1/Chassis/selected"]}

		macs, state := networkFunctionMACs(context.Background(), client, card)

		Expect(state.State).To(Equal("observed"))
		Expect(state.Tool).To(Equal("redfish"))
		Expect(macs).To(Equal([]string{"02:00:00:00:00:01", "f4:20:4d:16:19:b0"}))
		Expect(fixture.methods).NotTo(BeEmpty())
		for _, method := range fixture.methods {
			Expect(method).To(Equal(http.MethodGet))
		}
	})

	It("returns unknown when the selected card does not advertise adapters", func() {
		client, closeServer := networkGraphClient(newNetworkGraphFixture())
		defer closeServer()
		macs, state := networkFunctionMACs(context.Background(), client, Resource{URI: "/redfish/v1/Chassis/selected", Document: Document{}})
		Expect(macs).To(BeEmpty())
		Expect(state.State).To(Equal("unknown"))
		Expect(state.ReasonCode).To(Equal("LINK_NOT_EXPOSED"))
	})

	It("rejects foreign-origin and path-escape links throughout the graph", func() {
		for _, mode := range []string{"card-link", "adapter-link", "member-link"} {
			fixture := newNetworkGraphFixture()
			cardDocument := fixture.docs["/redfish/v1/Chassis/selected"]
			switch mode {
			case "card-link":
				cardDocument["NetworkAdapters"] = Document{"@odata.id": "https://foreign.invalid/redfish/v1/Chassis/selected/NetworkAdapters"}
			case "adapter-link":
				fixture.docs["/redfish/v1/Chassis/selected/NetworkAdapters/card0"]["NetworkDeviceFunctions"] = Document{"@odata.id": "/redfish/v1/Chassis/selected/../other/NetworkDeviceFunctions"}
			case "member-link":
				fixture.docs["/redfish/v1/Chassis/selected/NetworkAdapters"] = Document{"Members@odata.count": 1, "Members": []any{Document{"@odata.id": "https://foreign.invalid/redfish/v1/NetworkAdapters/card0"}}}
			}
			client, closeServer := networkGraphClient(fixture)
			card := Resource{URI: "/redfish/v1/Chassis/selected", Document: cardDocument}
			macs, state := networkFunctionMACs(context.Background(), client, card)
			closeServer()
			Expect(macs).To(BeEmpty())
			Expect(state.State).To(Equal("unavailable"))
			Expect(state.ReasonCode).To(Or(Equal("CROSS_ORIGIN_LINK"), Equal("UNSAFE_LINK"), Equal("UNSAFE_PAGINATION")))
		}
	})

	It("discards the graph when membership or permanent MAC identity changes on recheck", func() {
		for _, change := range []string{"membership", "mac"} {
			By(change)
			fixture := newNetworkGraphFixture()
			fixture.mutate = func(uri string, read int, doc Document) {
				if read <= 1 {
					return
				}
				switch {
				case change == "mac" && uri == "/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions/0":
					doc["Ethernet"] = Document{"PermanentMACAddress": "02:00:00:00:00:03"}
				case change == "membership" && uri == "/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions":
					doc["Members@odata.count"] = 3
					doc["Members"] = append(doc["Members"].([]any), Document{"@odata.id": "/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions/2"})
					fixture.docs["/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions/2"] = Document{"Id": "2", "NetDevFuncType": "Ethernet", "Ethernet": Document{"PermanentMACAddress": "02:00:00:00:00:03"}}
				}
			}
			client, closeServer := networkGraphClient(fixture)
			macs, state := networkFunctionMACs(context.Background(), client, Resource{URI: "/redfish/v1/Chassis/selected", Document: fixture.docs["/redfish/v1/Chassis/selected"]})
			closeServer()
			Expect(macs).To(BeEmpty())
			Expect(state.State).To(Equal("unavailable"))
			Expect(state.ReasonCode).To(Equal("IDENTITY_CHANGED"))
		}
	})

	It("rejects malformed, multicast, zero and duplicate MAC identities", func() {
		for _, item := range []struct {
			name   string
			first  string
			second string
		}{
			{name: "malformed", first: "02:00:00:00:00:0g", second: "02:00:00:00:00:01"},
			{name: "multicast", first: "01:00:00:00:00:01", second: "02:00:00:00:00:01"},
			{name: "zero", first: "00:00:00:00:00:00", second: "02:00:00:00:00:01"},
			{name: "duplicate", first: "02:00:00:00:00:01", second: "02:00:00:00:00:01"},
		} {
			By(item.name)
			fixture := newNetworkGraphFixture()
			fixture.docs["/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions/0"]["Ethernet"] = Document{"PermanentMACAddress": item.first}
			fixture.docs["/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions/1"]["Ethernet"] = Document{"PermanentMACAddress": item.second}
			client, closeServer := networkGraphClient(fixture)
			macs, state := networkFunctionMACs(context.Background(), client, Resource{URI: "/redfish/v1/Chassis/selected", Document: fixture.docs["/redfish/v1/Chassis/selected"]})
			closeServer()
			Expect(macs).To(BeEmpty())
			Expect(state.State).To(Equal("unavailable"))
		}
	})

	It("fails closed on denied and incomplete function collections", func() {
		for _, mode := range []string{"denied", "incomplete"} {
			By(mode)
			fixture := newNetworkGraphFixture()
			client, closeServer := networkGraphClient(fixture)
			if mode == "denied" {
				fixture.statuses["/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions"] = http.StatusForbidden
			} else {
				fixture.docs["/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions"] = Document{"Members@odata.count": 2, "Members": []any{Document{"@odata.id": "/redfish/v1/Chassis/selected/NetworkAdapters/card0/NetworkDeviceFunctions/0"}}}
			}
			macs, state := networkFunctionMACs(context.Background(), client, Resource{URI: "/redfish/v1/Chassis/selected", Document: fixture.docs["/redfish/v1/Chassis/selected"]})
			closeServer()
			Expect(macs).To(BeEmpty())
			Expect(state.State).To(Equal("unavailable"))
			if mode == "denied" {
				Expect(state.ReasonCode).To(Equal("ACCESS_DENIED"))
			} else {
				Expect(state.ReasonCode).To(Equal("INCOMPLETE_COLLECTION"))
			}
		}
	})
})
