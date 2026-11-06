// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var _ = Describe("API diagnostic persistence", func() {
	It("distinguishes authorization, absent resources, timeouts and cancellation without raw errors", func() {
		resource := schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}
		for _, tc := range []struct {
			err  error
			code string
		}{{apierrors.NewForbidden(resource, "x", fmt.Errorf("credential-canary")), "API_ACCESS_DENIED"}, {apierrors.NewUnauthorized("credential-canary"), "API_AUTHENTICATION_FAILED"}, {apierrors.NewNotFound(resource, "x"), "API_RESOURCE_NOT_FOUND"}, {context.DeadlineExceeded, "API_TIMEOUT"}, {context.Canceled, "API_CANCELLED"}, {fmt.Errorf("credential-canary"), "API_INSPECTION_FAILED"}} {
			o := apiObservationFailure(tc.err)
			Expect(o.ReasonCode).To(Equal(tc.code))
			Expect(o.Message).NotTo(BeEmpty())
			Expect(o.Message).NotTo(ContainSubstring("credential-canary"))
		}
	})
})
