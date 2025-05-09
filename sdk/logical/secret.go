// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package logical

import (
	"errors"
	"fmt"
)

// Secret represents the secret part of a response.
type Secret struct {
	LeaseOptions

	// InternalData is JSON-encodable data that is stored with the secret.
	// This will be sent back during a Renew/Revoke for storing internal data
	// used for those operations.
	InternalData map[string]interface{} `json:"internal_data" sentinel:""`

	// LeaseID is the ID returned to the user to manage this secret.
	// This is generated by Vault core. Any set value will be ignored.
	// For requests, this will always be blank.
	LeaseID string `sentinel:""`
}

func (s *Secret) Validate() error {
	if s.TTL < 0 {
		return errors.New("ttl duration must not be less than zero")
	}

	return nil
}

func (s *Secret) GoString() string {
	return fmt.Sprintf("*%#v", *s)
}
