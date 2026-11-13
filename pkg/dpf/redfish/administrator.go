// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"net/url"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// administrator binds the owned session's privileges to a matching local account.
// A username or a role name alone is not privilege evidence. Account documents
// and arbitrary OEM privileges are never retained in inventory.
func (c *Client) administrator(ctx context.Context, root Document) inventory.Observation {
	fail := func(code, message string) inventory.Observation {
		return inventory.Observation{State: "failed", ReasonCode: code, Message: message, Tool: "redfish"}
	}
	unavailable := func(err error) inventory.Observation {
		state := fail("BMC_ADMIN_UNVERIFIABLE", "The authenticated BMC account and its effective administrator privileges could not be verified ("+Code(err)+").")
		switch Code(err) {
		case "ACCESS_DENIED", "AUTHENTICATION_FAILED":
			state = fail("BMC_ADMIN_ACCESS_DENIED", "The BMC denied access needed to verify the authenticated account and its administrator privileges.")
		case "PASSWORD_CHANGE_REQUIRED":
			state = fail("BMC_PASSWORD_CHANGE_REQUIRED", "The BMC requires a password change before account access; discovery does not change passwords.")
		}
		return state
	}
	if c.passwordChangeRequired {
		return unavailable(&Error{Code: "PASSWORD_CHANGE_REQUIRED"})
	}
	username, _ := c.access.Credentials()
	session, err := c.Get(ctx, c.session)
	if err != nil {
		return unavailable(err)
	}
	if username == "" || Text(session, "UserName") != username {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC session does not identify the authenticated account consistently.")
	}
	service, err := c.accountLink(Link(root, "AccountService"), "/redfish/v1/AccountService")
	if err != nil {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC does not advertise a trusted standard AccountService link.")
	}
	doc, err := c.Get(ctx, service)
	if err != nil {
		return unavailable(err)
	}
	// AccountService policy can change after login. It can contradict local
	// authentication, but cannot prove the origin of this existing session.
	mode := ""
	if value, present := doc["LocalAccountAuth"]; present {
		var valid bool
		mode, valid = value.(string)
		if !valid || (mode != "Enabled" && mode != "Disabled" && mode != "Fallback" && mode != "LocalFirst") {
			return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC exposes an invalid local authentication mode.")
		}
		if mode == "Disabled" {
			return fail("BMC_ADMIN_UNVERIFIABLE", "Local BMC account authentication is disabled; a same-named local account cannot verify this session's privileges.")
		}
	}
	localSource := false
	if source, present := session["UserAuthenticationSource"]; present && source != nil {
		value, valid := source.(string)
		if !valid {
			return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC session exposes an invalid authentication source.")
		}
		if _, err := c.accountLink(value, service); err != nil {
			return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC session does not identify the trusted local AccountService as its authentication source; external accounts are not verified.")
		}
		localSource = true
	}
	if (mode == "Fallback" || mode == "LocalFirst") && !localSource {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC authentication mode permits external accounts, but the session does not establish a local authentication source.")
	}
	accountsURI, err := c.accountLink(Link(doc, "Accounts"), service+"/Accounts")
	if err != nil {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC does not advertise a trusted standard account collection.")
	}
	accounts, err := c.accounts(ctx, accountsURI, username)
	if err != nil {
		return unavailable(err)
	}
	var matches []Resource
	for _, account := range accounts {
		if Text(account.Document, "UserName") == username {
			matches = append(matches, account)
		}
	}
	if len(matches) != 1 {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The authenticated BMC username does not match exactly one local account; external or ambiguous accounts are not verified.")
	}
	account := matches[0]
	passwordChange, passwordKnown := account.Document["PasswordChangeRequired"].(bool)
	if passwordChange {
		return unavailable(&Error{Code: "PASSWORD_CHANGE_REQUIRED"})
	}
	enabled, enabledKnown := account.Document["Enabled"].(bool)
	locked, lockedKnown := account.Document["Locked"].(bool)
	if !passwordKnown || !enabledKnown || !lockedKnown {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC account does not expose valid enabled, locked and password-change state.")
	}
	if !enabled || locked {
		return fail("BMC_ADMIN_ACCOUNT_RESTRICTED", "The authenticated BMC account is disabled or locked.")
	}
	roleID := Text(account.Document, "RoleId")
	if roleID == "" || strings.ContainsAny(roleID, "/?#%") {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The authenticated BMC account does not expose a usable role identifier.")
	}
	if roles, present := session["Roles"]; present {
		values, valid := roles.([]any)
		if !valid || len(values) != 1 || values[0] != roleID {
			return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC session roles do not match the authenticated account's assigned role.")
		}
	} else if !localSource {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC session exposes neither a matching effective role nor verified local authentication; username equality alone does not establish privileges.")
	}
	roleURI, err := c.accountLink(Link(Object(account.Document, "Links"), "Role"), service+"/Roles/"+roleID)
	if err != nil {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The authenticated BMC account does not advertise a trusted link to its assigned role.")
	}
	role, err := c.Get(ctx, roleURI)
	if err != nil {
		return unavailable(err)
	}
	if Text(role, "Id") != roleID {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC role resource does not match the account's assigned role.")
	}
	assigned, ok := role["AssignedPrivileges"].([]any)
	if !ok {
		return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC role does not expose a valid standard privilege list.")
	}
	privileges := map[string]bool{}
	for _, value := range assigned {
		privilege, valid := value.(string)
		if !valid || privilege == "" || privileges[privilege] {
			return fail("BMC_ADMIN_UNVERIFIABLE", "The BMC role exposes a malformed standard privilege list.")
		}
		privileges[privilege] = true
	}
	// DMTF DSP0266: custom roles with the same privileges behave equivalently.
	for _, privilege := range []string{"Login", "ConfigureManager", "ConfigureUsers", "ConfigureComponents", "ConfigureSelf"} {
		if !privileges[privilege] {
			return fail("BMC_ADMIN_REQUIRED", "The authenticated BMC account lacks the full standard Administrator privilege set.")
		}
	}
	if c.passwordChangeRequired {
		return unavailable(&Error{Code: "PASSWORD_CHANGE_REQUIRED"})
	}
	return inventory.Observation{State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Message: "The BMC session has verified standard Administrator privileges; its matching local account is enabled, unlocked and requires no password change.", Tool: "redfish"}
}

// accounts selects an advertised username-addressed account without requiring
// permission to inspect unrelated accounts. Numeric/opaque account IDs use the
// complete bounded collection instead. Returned UserName is always verified by
// administrator; the URI spelling alone never establishes the account identity.
func (c *Client) accounts(ctx context.Context, uri, username string) ([]Resource, error) {
	doc, err := c.Get(ctx, uri)
	if err != nil {
		return nil, err
	}
	if next, present := doc["Members@odata.nextLink"]; present {
		if _, valid := next.(string); !valid {
			return nil, &Error{Code: "INVALID_RESPONSE"}
		}
	}
	members, valid := doc["Members"].([]any)
	count, counted := doc["Members@odata.count"].(float64)
	if !valid || !counted || count != float64(len(members)) || Text(doc, "Members@odata.nextLink") != "" {
		return c.collection(ctx, uri, uri+"/")
	}
	ownURI := uri + "/" + url.PathEscape(username)
	found := false
	seen := map[string]bool{}
	for _, member := range members {
		ref, err := c.guard(Link(Document{"member": member}, "member"))
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(ref, uri+"/") || strings.Contains(strings.TrimPrefix(ref, uri+"/"), "/") || strings.Contains(ref, "?") || ref == uri+"/" || seen[ref] {
			return nil, &Error{Code: "UNSAFE_ACCOUNT_LINK"}
		}
		seen[ref] = true
		found = found || ref == ownURI
	}
	if found {
		account, err := c.Get(ctx, ownURI)
		if err != nil {
			return nil, err
		}
		return []Resource{{URI: ownURI, Document: account}}, nil
	}
	return c.collection(ctx, uri, uri+"/")
}

// accountLink constrains account reads to the advertised standard resource tree,
// in addition to the transport's same-origin and traversal protections.
func (c *Client) accountLink(ref, expected string) (string, error) {
	uri, err := c.guard(ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSuffix(uri, "/") != expected {
		return "", &Error{Code: "UNSAFE_ACCOUNT_LINK"}
	}
	return expected, nil
}

func passwordChangeMessage(doc Document) bool {
	for _, container := range []Document{doc, Object(doc, "error")} {
		messages, _ := container["@Message.ExtendedInfo"].([]any)
		for _, item := range messages {
			id := Text(Object(Document{"message": item}, "message"), "MessageId")
			if strings.HasPrefix(id, "Base.") && strings.HasSuffix(id, ".PasswordChangeRequired") {
				return true
			}
		}
	}
	return false
}
