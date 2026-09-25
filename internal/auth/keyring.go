package auth

import "github.com/zalando/go-keyring"

// OSKeyring stores credentials in the platform credential manager.
type OSKeyring struct{}

func (OSKeyring) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}

func (OSKeyring) Get(service, account string) (string, error) {
	return keyring.Get(service, account)
}
