package models

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bluenviron/gortsplib/v4/pkg/base"
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
	DownloadURL string `json:"download_url"`
	ProgramName string `json:"program_name"`

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
	_, err := base.ParseURL(cfg.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("ivnalid address '%s' for component at path '%s': %w", cfg.DownloadURL, path, err)
	}
	if len(strings.TrimSpace(cfg.ProgramName)) <= 0 {
		return nil, fmt.Errorf("ivnalid program name '%s' for component at path '%s': %w", cfg.ProgramName, path, err)
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

func toggle_uac(enabled bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`, registry.WRITE)
	if err != nil {
		panic(err)
	}
	defer k.Close()
	dword := uint32(1)
	if !enabled {
		dword = 0
	}
	err = k.SetDWordValue("EnableLUA", dword)
	if err != nil {
		panic(err)
	}
}

func (s *windowsAutoupdateUpdater) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	key_name := `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, key_name, registry.READ)
	if err != nil {
		panic(err)
	}
	defer k.Close()

	subkeys, err := k.ReadSubKeyNames(0)
	if err != nil {
		panic(err)
	}
	for _, subkey := range subkeys {
		fmt.Printf("Checking key: %s\n", subkey)
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, fmt.Sprintf(`%s\%s`, key_name, subkey), registry.READ)
		if err != nil {
			panic(err)
		}
		defer sk.Close()

		publisher, _, err := sk.GetStringValue("Publisher")
		if err != nil {
			continue
		}
		if publisher == "Kongsberg Discovery Canada Ltd." {
			script, _, err := sk.GetStringValue("QuietUninstallString")
			if err != nil {
				panic(err)
			}
			fmt.Printf("\t%s\n", script)
			cmd := exec.Command("cmd", "/C", strings.ReplaceAll(script, `"`, ``))
			output, err := cmd.CombinedOutput()
			if err != nil {
				fmt.Println(string(output[:]))
				panic(err)
			}
			fmt.Println(string(output))
			fmt.Println("DONE")
			break
		}
	}
	return nil, nil
}

func (s *windowsAutoupdateUpdater) Close(context.Context) error {
	// Put close code here
	s.cancelFunc()
	return nil
}
