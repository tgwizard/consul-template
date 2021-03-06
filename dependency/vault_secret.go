package dependency

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Secret is a vault secret.
type Secret struct {
	LeaseID       string
	LeaseDuration int
	Renewable     bool

	// Data is the actual contents of the secret. The format of the data
	// is arbitrary and up to the secret backend.
	Data map[string]interface{}
}

// VaultSecret is the dependency to Vault for a secret
type VaultSecret struct {
	sync.Mutex

	Path   string
	secret *Secret

	stopped bool
	stopCh  chan struct{}
}

// Fetch queries the Vault API
func (d *VaultSecret) Fetch(clients *ClientSet, opts *QueryOptions) (interface{}, *ResponseMetadata, error) {
	d.Lock()
	if d.stopped {
		defer d.Unlock()
		return nil, nil, ErrStopped
	}
	d.Unlock()

	if opts == nil {
		opts = &QueryOptions{}
	}

	log.Printf("[DEBUG] (%s) querying vault with %+v", d.Display(), opts)

	// If this is not the first query and we have a lease duration, sleep until we
	// try to renew.
	if opts.WaitIndex != 0 && d.secret != nil && d.secret.LeaseDuration != 0 {
		duration := time.Duration(d.secret.LeaseDuration/2) * time.Second
		log.Printf("[DEBUG] (%s) pretending to long-poll for %q",
			d.Display(), duration)
		select {
		case <-d.stopCh:
			log.Printf("[DEBUG] (%s) received interrupt", d.Display())
			return nil, nil, ErrStopped
		case <-time.After(duration):
		}
	}

	// Grab the vault client
	vault, err := clients.Vault()
	if err != nil {
		return nil, nil, fmt.Errorf("vault secret: %s", err)
	}

	// Attempt to renew the secret. If we do not have a secret or if that secret
	// is not renewable, we will attempt a (re-)read later.
	if d.secret != nil && d.secret.LeaseID != "" && d.secret.Renewable {
		renewal, err := vault.Sys().Renew(d.secret.LeaseID, 0)
		if err == nil {
			log.Printf("[DEBUG] (%s) successfully renewed", d.Display())

			secret := &Secret{
				LeaseID:       renewal.LeaseID,
				LeaseDuration: leaseDurationOrDefault(renewal.LeaseDuration),
				Renewable:     renewal.Renewable,
				Data:          d.secret.Data,
			}

			d.Lock()
			d.secret = secret
			d.Unlock()

			return respWithMetadata(secret)
		}

		// The renewal failed for some reason.
		log.Printf("[WARN] (%s) failed to renew, re-reading: %s", d.Display(), err)
	}

	// If we got this far, we either didn't have a secret to renew, the secret was
	// not renewable, or the renewal failed, so attempt a fresh read.
	vaultSecret, err := vault.Logical().Read(d.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("error reading from vault: %s", err)
	}

	// The secret could be nil (maybe it does not exist yet). This is not an error
	// to Vault, but it is an error to Consul Template, so return an error
	// instead.
	if vaultSecret == nil {
		return nil, nil, fmt.Errorf("no secret exists at path %q", d.Display())
	}

	// Create our cloned secret
	secret := &Secret{
		LeaseID:       vaultSecret.LeaseID,
		LeaseDuration: leaseDurationOrDefault(vaultSecret.LeaseDuration),
		Renewable:     vaultSecret.Renewable,
		Data:          vaultSecret.Data,
	}

	d.Lock()
	d.secret = secret
	d.Unlock()

	log.Printf("[DEBUG] (%s) vault returned the secret", d.Display())

	return respWithMetadata(secret)
}

// CanShare returns if this dependency is shareable.
func (d *VaultSecret) CanShare() bool {
	return false
}

// HashCode returns the hash code for this dependency.
func (d *VaultSecret) HashCode() string {
	return fmt.Sprintf("VaultSecret|%s", d.Path)
}

// Display returns a string that should be displayed to the user in output (for
// example).
func (d *VaultSecret) Display() string {
	return fmt.Sprintf(`"secret(%s)"`, d.Path)
}

// Stop halts the given dependency's fetch.
func (d *VaultSecret) Stop() {
	d.Lock()
	defer d.Unlock()

	if !d.stopped {
		close(d.stopCh)
		d.stopped = true
	}
}

// ParseVaultSecret creates a new datacenter dependency.
func ParseVaultSecret(s string) (*VaultSecret, error) {
	vs := &VaultSecret{
		Path:   s,
		stopCh: make(chan struct{}),
	}
	return vs, nil
}

func leaseDurationOrDefault(d int) int {
	if d == 0 {
		return 5 * 60
	}
	return d
}
