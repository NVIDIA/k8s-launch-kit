package spectrumx

import (
	"reflect"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func TestPoolSettingsIPv4GatewayIndex(t *testing.T) {
	spcx := &config.ProfileSpectrumX{IPVersion: config.SpectrumXIPVersionIPv4}
	got := poolSettings(spcx)
	want := cidrPoolSettings{
		gatewayIndex:         1,
		perNodeNetworkPrefix: 31,
		perNodeExclusions:    []PerNodeExclusion{{StartIndex: 1, EndIndex: 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("poolSettings(IPv4) = %+v, want %+v", got, want)
	}
}

func TestPoolSettingsIPv6Unchanged(t *testing.T) {
	spcx := &config.ProfileSpectrumX{IPVersion: config.SpectrumXIPVersionIPv6}
	got := poolSettings(spcx)
	want := cidrPoolSettings{
		gatewayIndex:         2,
		perNodeNetworkPrefix: 64,
		perNodeExclusions:    []PerNodeExclusion{{StartIndex: 2, EndIndex: 2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("poolSettings(IPv6) = %+v, want %+v", got, want)
	}
}
