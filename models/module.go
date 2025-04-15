package models

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cavaliergopher/grab/v3"
	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/utils"
	"golang.org/x/sys/windows/registry"
)

var (
	Updater = resource.NewModel("njooma", "windows_autoupdate", "updater")
)

func init() {
	resource.RegisterComponent(generic.API, Updater,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newWindowsAutoupdateUpdater,
		},
	)
}

type Config struct {
	DownloadURL         string   `json:"download_url"`
	InstallerPath       string   `json:"installer_path"`
	InstallArgs         []string `json:"install_args"`
	RegistryLookupKey   string   `json:"registry_lookup_key"`
	RegistryLookupValue string   `json:"registry_lookup_value"`
}

func (cfg *Config) Validate(path string) ([]string, error) {
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

	downloadWorkers  utils.StoppableWorkers
	downloadComplete bool

	resource.AlwaysRebuild
}

func newWindowsAutoupdateUpdater(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	s := &windowsAutoupdateUpdater{
		name:             rawConf.ResourceName(),
		logger:           logger,
		cfg:              conf,
		downloadWorkers:  *utils.NewBackgroundStoppableWorkers(),
		downloadComplete: false,
	}

	s.downloadWorkers.Add(s.downloadIgnoringReturn)

	return s, nil
}

func (s *windowsAutoupdateUpdater) Name() resource.Name {
	return s.name
}

func (s *windowsAutoupdateUpdater) downloadIgnoringReturn(ctx context.Context) {
	s.downloadComplete = false
	s.downloadUpdate(ctx)
	s.downloadComplete = true
}

func (s *windowsAutoupdateUpdater) downloadUpdate(ctx context.Context) (string, error) {
	client := grab.NewClient()
	req, err := grab.NewRequest(".", s.cfg.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("could not create request: %w", err)
	}
	req = req.WithContext(ctx)

	// start download
	s.logger.Infof("downloading update from: %v", req.URL())
	resp := client.Do(req)

	// start status loop
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

Loop:
	for {
		select {
		case <-t.C:
			s.logger.Debugf("downloaded %v / %v bytes (%.2f%%)", resp.BytesComplete(), resp.Size(), 100*resp.Progress())
		case <-resp.Done:
			s.logger.Debugf("downloaded %v / %v bytes (%.2f%%)", resp.BytesComplete(), resp.Size(), 100*resp.Progress())
			break Loop
		}
	}

	// check for errors
	if err := resp.Err(); err != nil {
		return "", fmt.Errorf("could not download file: %w", err)
	}

	// success
	fmt.Printf("update saved to %s", resp.Filename)
	return resp.Filename, nil
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

// Find the installer in the downloaded update.
// If the downloaded update is a zip, unzip first.
// Search for something that looks like an installer.
// Return the installer, the (unzipped) directory if in a folder, and error
func (s *windowsAutoupdateUpdater) findInstaller(src string) (string, string, error) {

	extensions := []string{".exe", ".msi", ".bat"}

	if path.Ext(src) == ".zip" {
		s.logger.Info("update is a zip file, unzipping...")
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
		s.logger.Info("Skipping uninstall: Registry lookup key was not provided.")
		return nil
	}
	if len(strings.TrimSpace(s.cfg.RegistryLookupValue)) <= 0 {
		s.logger.Info("Skipping uninstall: Registry lookup value was not provided.")
		return nil
	}

	s.logger.Infof("uninstalling program with registry key %s: %s", s.cfg.RegistryLookupKey, s.cfg.RegistryLookupValue)
	keys := []string{
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
	}

	uninstallCount := 0
	errors := []error{}
	for _, key_name := range keys {
		s.logger.Infof("checking registry: %s", key_name)
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, key_name, registry.READ)
		if err != nil {
			s.logger.Infof("error checking registry at %s: %w", key_name, err)
			continue
		}
		defer k.Close()

		subkeys, err := k.ReadSubKeyNames(0)
		if err != nil {
			s.logger.Infof("error getting subkeys for %s: %w", key_name, err)
			continue
		}
		for _, subkey := range subkeys {
			s.logger.Infof("checking subkey: %s", subkey)
			sk, err := registry.OpenKey(registry.LOCAL_MACHINE, fmt.Sprintf(`%s\%s`, key_name, subkey), registry.READ)
			if err != nil {
				s.logger.Infof("error opening subkey %s: %w", subkey, err)
				continue
			}
			defer sk.Close()

			lookupValue, _, err := sk.GetStringValue(s.cfg.RegistryLookupKey)
			if err != nil {
				s.logger.Infof("error getting value %s: %w", s.cfg.RegistryLookupKey, err)
				continue
			}
			if lookupValue == s.cfg.RegistryLookupValue {
				script, _, err := sk.GetStringValue("QuietUninstallString")
				if err != nil || len(strings.TrimSpace(script)) <= 0 {
					script, _, err = sk.GetStringValue("UninstallString")
					if err != nil {
						errors = append(errors, fmt.Errorf("could not find uninstall command: %w", err))
						s.logger.Errorf("could not find uninstall command: %w", err)
						continue
					}
				}
				s.logger.Infof("running uninstall command: %s", script)
				cmd := exec.Command("cmd", "/C", strings.ReplaceAll(script, `"`, ``))
				output, err := cmd.CombinedOutput()
				if err != nil {
					errors = append(errors, fmt.Errorf("encountered error uninstalling program: %s", string(output[:])))
					s.logger.Errorf("encountered error uninstalling program: %s", string(output[:]))
				}
				uninstallCount++
				s.logger.Infof("successfully uninstalled: %s", string(output[:]))
			}
		}
	}
	if uninstallCount > 0 {
		s.logger.Infof("uninstalled %d programs", uninstallCount)
	} else {
		s.logger.Info("existing installation not found")
	}
	if len(errors) > 0 {
		return fmt.Errorf("encountered errors uninstalling programs: %v", errors)
	}
	return nil
}

func (s *windowsAutoupdateUpdater) installUpdate(installer string) error {
	s.logger.Infof("installing update from %s", installer)
	args := append([]string{"/C", installer}, s.cfg.InstallArgs...)
	cmd := exec.Command("cmd", args...)
	s.logger.Infof("installation command: %s", s.cfg.InstallArgs)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("encountered error installing program: %s", string(output[:]))
	}
	s.logger.Infof("successfully installed: %s", string(output[:]))
	return nil
}

func (s *windowsAutoupdateUpdater) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	for utils.SelectContextOrWait(ctx, 1*time.Second) {
		if s.downloadComplete {
			break
		}
	}
	update, err := s.downloadUpdate(ctx)
	if err != nil {
		return nil, err
	}
	defer os.Remove(update)

	// Chcek if installer exists before uninstalling anything
	installer, dir, err := s.findInstaller(update)
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
	s.downloadWorkers.Stop()
	return nil
}
