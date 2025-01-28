package models

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
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
	DownloadURL         string   `json:"download_url"`
	InstallerPath       string   `json:"installer_path"`
	InstallArgs         []string `json:"install_args"`
	RegistryLookupKey   string   `json:"registry_lookup_key"`
	RegistryLookupValue string   `json:"registry_lookup_value"`

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
	newConf, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}
	s.cfg = newConf
	return nil
}

func (s *windowsAutoupdateUpdater) downloadUpdate(ctx context.Context) (*os.File, error) {
	s.logger.Debugf("downloading update from: %s", s.cfg.DownloadURL)
	filename := path.Base(s.cfg.DownloadURL)
	file, err := os.CreateTemp(".", fmt.Sprintf("*-%s", filename))
	if err != nil {
		return nil, fmt.Errorf("could not create download file: %w", err)
	}
	defer file.Close()

	s.logger.Debugf("destination for download: %s", file.Name())
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

	s.logger.Debug("successfully downloaded update, copying...")
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("could not save downloaded file: %w", err)
	}
	s.logger.Debug("updated saved")
	return file, nil
}

// Find the installer in the downloaded update.
// If the downloaded update is a zip, unzip first.
// Search for something that looks like an installer.
// Return the installer, the (unzipped) directory if in a folder, and error
func (s *windowsAutoupdateUpdater) findInstaller(src string) (string, string, error) {

	extensions := []string{".exe", ".msi", ".bat"}

	if path.Ext(src) == ".zip" {
		s.logger.Debug("update is a zip file, unzipping...")
		dest := strings.TrimSuffix(src, path.Ext(src))
		err := unzipUpdate(src, dest)
		if err != nil {
			os.RemoveAll(dest)
			return "", "", err
		}
		src = dest
	}

	desc, err := os.Stat(src)
	if err != nil {
		return "", "", err
	}

	if desc.IsDir() {
		if s.cfg.InstallerPath != "" {
			installerPath := filepath.Join(desc.Name(), s.cfg.InstallerPath)
			if _, err := os.Stat(installerPath); err != nil {
				os.RemoveAll(desc.Name())
				return "", "", fmt.Errorf("could not find installer at %s: %w", installerPath, err)
			}
			return installerPath, desc.Name(), nil
		}

		files, err := os.ReadDir(desc.Name())
		if err != nil {
			os.RemoveAll(desc.Name())
			return "", "", err
		}
		for _, file := range files {
			if slices.Contains(extensions, path.Ext(file.Name())) {
				return filepath.Join(desc.Name(), file.Name()), desc.Name(), nil
			}
		}
	} else {
		if slices.Contains(extensions, path.Ext(desc.Name())) {
			return desc.Name(), "", nil
		}
	}
	return "", "", errors.New("could not find a file that resembles an installer")
}

func (s *windowsAutoupdateUpdater) uninstallExistingInstallation() error {
	// Skip uninstall step if these config values are not provided
	if len(strings.TrimSpace(s.cfg.RegistryLookupKey)) <= 0 {
		s.logger.Debug("Skipping uninstall: Registry lookup key was not provided.")
		return nil
	}
	if len(strings.TrimSpace(s.cfg.RegistryLookupValue)) <= 0 {
		s.logger.Debug("Skipping uninstall: Registry lookup value was not provided.")
		return nil
	}

	s.logger.Debugf("uninstalling program with registry key %s: %s", s.cfg.RegistryLookupKey, s.cfg.RegistryLookupValue)
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
	// Existing installation not found
	s.logger.Debug("existing installation not found")
	return nil
}

func unzipUpdate(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	os.MkdirAll(dest, 0755)

	extractAndWriteFile := func(f *zip.File) error {
		rc, err := f.Open()
		if err != nil {
			os.RemoveAll(dest)
			return err
		}
		defer rc.Close()

		p := filepath.Join(dest, f.Name)

		// Check for ZipSlip (Directory traversal)
		if !strings.HasPrefix(p, filepath.Clean(dest)+string(os.PathSeparator)) {
			os.RemoveAll(dest)
			return fmt.Errorf("illegal file path: %s", p)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(p, f.Mode())
		} else {
			os.MkdirAll(filepath.Dir(p), f.Mode())
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				os.RemoveAll(dest)
				return err
			}
			defer f.Close()

			_, err = io.Copy(f, rc)
			if err != nil {
				os.RemoveAll(dest)
				return err
			}
		}
		return nil
	}

	for _, f := range r.File {
		err := extractAndWriteFile(f)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *windowsAutoupdateUpdater) installUpdate(installer string) error {
	s.logger.Debugf("installing update from %s", installer)
	args := append([]string{"/C", installer}, s.cfg.InstallArgs...)
	cmd := exec.Command("cmd", args...)
	s.logger.Debugf("installation command: %s", s.cfg.InstallArgs)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("encountered error installing program: %s", string(output[:]))
	}
	s.logger.Debugf("successfully installed: %s", string(output[:]))
	return nil
}

func (s *windowsAutoupdateUpdater) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	update, err := s.downloadUpdate(ctx)
	if err != nil {
		return nil, err
	}
	defer os.Remove(update.Name())

	// Chcek if installer exists before uninstalling anything
	installer, dir, err := s.findInstaller(update.Name())
	if err != nil {
		return nil, err
	}
	defer func() {
		if dir != "" {
			os.RemoveAll(dir)
		} else {
			os.RemoveAll(installer)
		}
	}()

	if err := s.uninstallExistingInstallation(); err != nil {
		return nil, err
	}

	if err := s.installUpdate(installer); err != nil {
		return nil, err
	}

	return nil, nil
}

func (s *windowsAutoupdateUpdater) Close(context.Context) error {
	// Put close code here
	s.cancelFunc()
	return nil
}
