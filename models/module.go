package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"golang.org/x/sys/windows/registry"
)

var (
	Updater          = resource.NewModel("njooma", "windows_autoupdate", "updater")
	errUnimplemented = errors.New("unimplemented")
)

func init() {
	resource.RegisterComponent(generic.API, Updater,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newWindowsAutoupdateUpdater,
		},
	)
}

type Config struct {
	/*
		Put config attributes here. There should be public/exported fields
		with a `json` parameter at the end of each attribute.

		Example config struct:
			type Config struct {
				Pin   string `json:"pin"`
				Board string `json:"board"`
				MinDeg *float64 `json:"min_angle_deg,omitempty"`
			}

		If your model does not need a config, replace *Config in the init
		function with resource.NoNativeConfig
	*/
	DownloadURL         string `json:"download_url"`
	InstallerPath       string `json:"installer_path"`
	RegistryLookupKey   string `json:"registry_lookup_key"`
	RegistryLookupValue string `json:"registry_lookup_value"`

	/* Uncomment this if your model does not need to be validated
	   and has no implicit dependecies. */
	// resource.TriviallyValidateConfig
}

// Validate ensures all parts of the config are valid and important fields exist.
// Returns implicit dependencies based on the config.
// The path is the JSON path in your robot's config (not the `Config` struct) to the
// resource being validated; e.g. "components.0".
func (cfg *Config) Validate(path string) ([]string, error) {
	// Add config validation code here
	_, err := url.Parse(cfg.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("ivnalid address '%s' for component at path '%s': %w", cfg.DownloadURL, path, err)
	}
	if len(strings.TrimSpace(cfg.RegistryLookupKey)) <= 0 {
		return nil, fmt.Errorf("ivnalid registry key '%s' for component at path '%s': %w", cfg.RegistryLookupKey, path, err)
	}
	if len(strings.TrimSpace(cfg.RegistryLookupValue)) <= 0 {
		return nil, fmt.Errorf("ivnalid registry value '%s' for component at path '%s': %w", cfg.RegistryLookupValue, path, err)
	}
	return nil, nil
}

type windowsAutoupdateUpdater struct {
	name resource.Name

	logger logging.Logger
	cfg    *Config

	cancelCtx  context.Context
	cancelFunc func()

	/* Uncomment this if your model does not need to reconfigure. */
	resource.TriviallyReconfigurable

	// Uncomment this if the model does not have any goroutines that
	// need to be shut down while closing.
	resource.TriviallyCloseable
}

func newWindowsAutoupdateUpdater(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &windowsAutoupdateUpdater{
		name:       rawConf.ResourceName(),
		logger:     logger,
		cfg:        conf,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}
	return s, nil
}

func (s *windowsAutoupdateUpdater) Name() resource.Name {
	return s.name
}

func (s *windowsAutoupdateUpdater) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	// Put reconfigure code here
	return errUnimplemented
}

func (s *windowsAutoupdateUpdater) downloadUpdate(ctx context.Context) (*os.File, error) {
	filename := path.Base(s.cfg.DownloadURL)
	file, err := os.CreateTemp(".", fmt.Sprintf("*-%s", filename))
	if err != nil {
		return nil, fmt.Errorf("could not create download file: %w", err)
	}
	defer file.Close()

	request, err := http.NewRequestWithContext(ctx, "GET", s.cfg.DownloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("could ont create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("could not download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading file resulted in non-OK status: %s", resp.Status)
	}

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("could not save downloaded file: %w", err)
	}
	return file, nil
}

func (s *windowsAutoupdateUpdater) uninstallExistingInstallation() error {
	keys := []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	}
	for _, key_name := range keys {
		s.logger.Debugf("checking registry: %s", key_name)
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, key_name, registry.READ)
		if err != nil {
			s.logger.Debugf("error checking registry at %s: %w", key_name, err)
			continue
		}
		defer k.Close()

		subkeys, err := k.ReadSubKeyNames(0)
		if err != nil {
			s.logger.Debugf("error getting subkeys for %s: %w", key_name, err)
			continue
		}
		for _, subkey := range subkeys {
			s.logger.Debugf("checking subkey: %s", subkey)
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, fmt.Sprintf(`%s\%s`, key_name, subkey), registry.READ)
			if err != nil {
				s.logger.Debugf("error opening subkey %s: %w", subkey, err)
				continue
			}
			defer sk.Close()

			lookupValue, _, err := sk.GetStringValue(s.cfg.RegistryLookupKey)
			if err != nil {
				s.logger.Debugf("error getting value %s: %w", s.cfg.RegistryLookupKey, err)
				continue
			}
			if lookupValue == s.cfg.RegistryLookupValue {
				script, _, err := sk.GetStringValue("QuietUninstallString")
				if err != nil || len(strings.TrimSpace(script)) <= 0 {
					script, _, err = sk.GetStringValue("UninstallString")
					if err != nil {
						return fmt.Errorf("could not find uninstall command: %w", err)
					}
				}
				s.logger.Debugf("running uninstall command: %s", script)
				cmd := exec.Command("cmd", "/C", strings.ReplaceAll(script, `"`, ``))
				output, err := cmd.CombinedOutput()
				if err != nil {
					return fmt.Errorf("encountered error uninstalling program: %s", string(output[:]))
				}
				s.logger.Debugf("successfully uninstalled: %s", string(output[:]))
				return nil
			}
		}
	}
	return errors.New("could not uninstall existing installation")
}

func (s *windowsAutoupdateUpdater) installUpdate(ctx context.Context, update *os.File) error {
	s.logger.Warnf("update downloaded to: %s", update.Name())
	return nil
}

func (s *windowsAutoupdateUpdater) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	update, err := s.downloadUpdate(ctx)
	defer os.Remove(update.Name())
	if err != nil {
		return nil, err
	}
	if err := s.uninstallExistingInstallation(); err != nil {
		return nil, err
	}
	if err := s.installUpdate(ctx, update); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *windowsAutoupdateUpdater) Close(context.Context) error {
	// Put close code here
	s.cancelFunc()
	return nil
}
