// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: FSL-1.1-ALv2

// Command terraform-provider-testprov is a protocol 6 OpenTofu provider
// built on terraform-plugin-framework, used only by provider-bridge's tests.
// It talks to no API: Read derives every attribute from the resource ID.
//
// It lives in its own Go module so that terraform-plugin-go, which the
// framework depends on, never enters the bridge's module graph.
package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

func main() {
	err := providerserver.Serve(context.Background(), newProvider, providerserver.ServeOpts{
		Address:         "registry.opentofu.org/sanoma/testprov",
		ProtocolVersion: 6,
	})
	if err != nil {
		log.Fatal(err)
	}
}
