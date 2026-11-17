// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

// DeviceOutput is the v3 public host/DPU contract. ID is published only after
// physical identity rechecks succeed; an endpoint or retained serial is context,
// not a successful identity assertion. Collector bookkeeping stays private.
type DeviceOutput struct {
	RecordKey                 string          `json:"recordKey" yaml:"recordKey"`
	InputRef                  string          `json:"inputRef" yaml:"inputRef"`
	Method                    string          `json:"method,omitempty" yaml:"method,omitempty"`
	ID                        string          `json:"id,omitempty" yaml:"id,omitempty"`
	BMCEndpoint               string          `json:"bmcEndpoint,omitempty" yaml:"bmcEndpoint,omitempty"`
	SSHEndpoint               string          `json:"sshEndpoint,omitempty" yaml:"sshEndpoint,omitempty"`
	SystemURI                 string          `json:"systemURI,omitempty" yaml:"systemURI,omitempty"`
	SerialNumber              string          `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	SystemUUID                string          `json:"systemUUID,omitempty" yaml:"systemUUID,omitempty"`
	Manufacturer              string          `json:"manufacturer,omitempty" yaml:"manufacturer,omitempty"`
	Model                     string          `json:"model,omitempty" yaml:"model,omitempty"`
	PSID                      string          `json:"psid,omitempty" yaml:"psid,omitempty"`
	BMCAdministratorVerified  bool            `json:"bmcAdministratorVerified,omitempty" yaml:"bmcAdministratorVerified,omitempty"`
	FirmwareInventoryComplete bool            `json:"firmwareInventoryComplete,omitempty" yaml:"firmwareInventoryComplete,omitempty"`
	Firmware                  *FirmwareOutput `json:"firmware,omitempty" yaml:"firmware,omitempty"`
	Runtime                   *RuntimeOutput  `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	Adapters                  []AdapterOutput `json:"adapters,omitempty" yaml:"adapters,omitempty"`
	AdvertisedResetTypes      []string        `json:"advertisedResetTypes,omitempty" yaml:"advertisedResetTypes,omitempty"`
	Issues                    []DeviceIssue   `json:"issues,omitempty" yaml:"issues,omitempty"`
}

// DeviceIssue describes an unsuccessful read, missing fact or mismatch. It never
// stores successful parameter values or an observation success state.
type DeviceIssue struct {
	Field   string `json:"field" yaml:"field"`
	Code    string `json:"code" yaml:"code"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
	Tool    string `json:"tool,omitempty" yaml:"tool,omitempty"`
}

// FirmwareOutput names only meaningful DPU components. Versions with unknown
// activation remain distinguishable from running and explicitly staged images.
// Unclassified vendor entries and host BMC firmware inventories are not exported.
type FirmwareOutput struct {
	BMC     []FirmwareVersionOutput `json:"bmc,omitempty" yaml:"bmc,omitempty"`
	NIC     []FirmwareVersionOutput `json:"nic,omitempty" yaml:"nic,omitempty"`
	CEC     []FirmwareVersionOutput `json:"cec,omitempty" yaml:"cec,omitempty"`
	ATF     []FirmwareVersionOutput `json:"atf,omitempty" yaml:"atf,omitempty"`
	UEFI    []FirmwareVersionOutput `json:"uefi,omitempty" yaml:"uefi,omitempty"`
	ATFUEFI []FirmwareVersionOutput `json:"atfUEFI,omitempty" yaml:"atfUEFI,omitempty"`
	BSP     []FirmwareVersionOutput `json:"bsp,omitempty" yaml:"bsp,omitempty"`
	OS      []FirmwareVersionOutput `json:"os,omitempty" yaml:"os,omitempty"`
	Golden  []FirmwareVersionOutput `json:"golden,omitempty" yaml:"golden,omitempty"`
	BMCERoT []FirmwareVersionOutput `json:"bmcERoT,omitempty" yaml:"bmcERoT,omitempty"`
	CPUERoT []FirmwareVersionOutput `json:"cpuERoT,omitempty" yaml:"cpuERoT,omitempty"`
}

type FirmwareVersionOutput struct {
	Version    string `json:"version" yaml:"version"`
	Activation string `json:"activation,omitempty" yaml:"activation,omitempty"`
}

type RuntimeOutput struct {
	Hostname     string              `json:"hostname,omitempty" yaml:"hostname,omitempty"`
	OS           string              `json:"os,omitempty" yaml:"os,omitempty"`
	BoardProduct string              `json:"boardProduct,omitempty" yaml:"boardProduct,omitempty"`
	MFTVersion   string              `json:"mftVersion,omitempty" yaml:"mftVersion,omitempty"`
	Interfaces   []Interface         `json:"interfaces,omitempty" yaml:"interfaces,omitempty"`
	Routes       []Route             `json:"routes,omitempty" yaml:"routes,omitempty"`
	DNS          []string            `json:"dns,omitempty" yaml:"dns,omitempty"`
	Network      *NetworkObservation `json:"network,omitempty" yaml:"network,omitempty"`
	Hardware     *HardwareOutput     `json:"hardware,omitempty" yaml:"hardware,omitempty"`
}

type AdapterOutput struct {
	Key          string        `json:"adapterKey" yaml:"adapterKey"`
	SerialNumber string        `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	Model        string        `json:"model,omitempty" yaml:"model,omitempty"`
	Slot         string        `json:"slot,omitempty" yaml:"slot,omitempty"`
	PCIAddress   string        `json:"pciAddress,omitempty" yaml:"pciAddress,omitempty"`
	VendorID     string        `json:"vendorID,omitempty" yaml:"vendorID,omitempty"`
	DeviceID     string        `json:"deviceID,omitempty" yaml:"deviceID,omitempty"`
	PCIFunctions []PCIFunction `json:"pciFunctions,omitempty" yaml:"pciFunctions,omitempty"`
}

type HardwareOutput struct {
	Cards []CardOutput        `json:"cards,omitempty" yaml:"cards,omitempty"`
	RShim *RShimServiceOutput `json:"rshim,omitempty" yaml:"rshim,omitempty"`
}

type CardOutput struct {
	PCIAddress      string                `json:"pciAddress" yaml:"pciAddress"`
	Domain          string                `json:"domain" yaml:"domain"`
	Model           string                `json:"model,omitempty" yaml:"model,omitempty"`
	SerialNumber    string                `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	FirmwareVersion string                `json:"firmwareVersion,omitempty" yaml:"firmwareVersion,omitempty"`
	PSID            string                `json:"psid,omitempty" yaml:"psid,omitempty"`
	NextBoot        *NVConfigOutput       `json:"nextBoot,omitempty" yaml:"nextBoot,omitempty"`
	HostPrivileges  []HostPrivilegeOutput `json:"hostPrivileges,omitempty" yaml:"hostPrivileges,omitempty"`
}

// NextBoot describes the mlxconfig next-boot columns, never the running mode.
type NVConfigOutput struct {
	InternalCPUOffloadEngine string  `json:"internalCPUOffloadEngine,omitempty" yaml:"internalCPUOffloadEngine,omitempty"`
	InternalCPURShim         string  `json:"internalCPURShim,omitempty" yaml:"internalCPURShim,omitempty"`
	NumVFs                   *uint32 `json:"numVFs,omitempty" yaml:"numVFs,omitempty"`
}

type HostPrivilegeOutput struct {
	HostIndex          uint32 `json:"hostIndex" yaml:"hostIndex"`
	Level              string `json:"level" yaml:"level"`
	DisableRShim       *bool  `json:"disableRShim,omitempty" yaml:"disableRShim,omitempty"`
	DisableTracer      *bool  `json:"disableTracer,omitempty" yaml:"disableTracer,omitempty"`
	DisablePortOwner   *bool  `json:"disablePortOwner,omitempty" yaml:"disablePortOwner,omitempty"`
	DisableCounterRead *bool  `json:"disableCounterRead,omitempty" yaml:"disableCounterRead,omitempty"`
}

type RShimServiceOutput struct {
	LoadState   string `json:"loadState" yaml:"loadState"`
	ActiveState string `json:"activeState" yaml:"activeState"`
}
