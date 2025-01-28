# Windows Auto-Updater
### Update an application on a Windows machine

## Description
Update your Windows applications. Will try to
1. Download the update from the specified URL
1. Uninstall an existing installation of the same program
1. Install the update

This module tries to do everything silently, but there might be UAC prompts if the module is not run in administrator mode.

## Attributes
### `download_url` 
**REQUIRED** 
The URL of the update

### `installer_path` 
**OPTIONAL** default: `<empty string>`

The path of the installer if the download URL points to a directory. This value should be relative to the root of the downloaded directory. If the downloaded update is a zip, the module will unzip first.

This is optional because the module will search for an installer (extensions `.exe`, `.msi`, or `.bat`) in the downloaded directory, but ONLY ONE LEVEL DEEP. So if the zip file stores the installer in a nested directory, you should add the `installer_path` attribute.

For example, if the downloaded update has a directory structure as follows:
```
downloaded_directory/
├─ README.md
├─ Release-Notes.txt
├─ setup.exe
```
The module will find the `setup.exe` installer and run it.

However, if the downloaded update has the directory structure
```
downloaded_directory/
├─ subdirectory/
│  ├─ README.md
│  ├─ Release-Notes.txt
│  ├─ setup.exe
```
the module will NOT find the `setup.exe` installer. In this case, you should specify the `installer_path` attribute as `subdirectory\setup.exe`.

### `install_args` 
**OPTIONAL** default: `<empty array>`

Array of arguments to pass to the installer. For example, `["/quiet", "/norestart"]`. Note: not all installers support arguments.

### `registry_lookup_key`
**OPTIONAL** default: `<empty string>`

Used in conjunction with `registry_lookup_value` in order to identify the existing installation in the registry to uninstall it. The module will uninstall the first application found that matches these parameters. If not provided, the module will skip the uninstall step.

For example, you could provide the `registry_lookup_key` of "DisplayName", with the associated `registry_lookup_value` of "Some Example Program" in order to uninstall "Some Example Program". 

### `registry_lookup_value`
**OPTIONAL** default: `<empty string>`

Used in conjunction with `registry_lookup_key` in order to identify the existing installation in the registry to uninstall it. The module will uninstall the first application found that matches these parameters. If not provided, the module will skip the uninstall step.

For example, you could provide the `registry_lookup_key` of "DisplayName", with the associated `registry_lookup_value` of "Some Example Program" in order to uninstall "Some Example Program". 
