# k223fetch

This program was created entirely using `GPT-5.6-Sol` without writing code by hand.

`k223fetch` downloads the original `K223RGB_V10003.bin` firmware
for the **FL·ESPORTS GP108** keyboard from known vendor packages and mirrors.
It is a **Witmod** firmware targeting the keyboard's **Nuvoton M252SD2AE**
microcontroller.

The utility is available for Windows, macOS, and Linux on amd64 and arm64.

## Download firmware on Windows

1. Download the Windows ZIP from
   [GitHub Releases](https://github.com/Belonit/k223fetch/releases): use
   `windows-amd64` for a regular Intel/AMD PC or `windows-arm64` for Windows on
   ARM.
2. Extract the entire archive to a directory. Do not separate the executable
   from `sources.json`.
3. Open PowerShell in that directory and run:

```powershell
.\k223fetch-windows-amd64.exe
```

For Windows on ARM, run `k223fetch-windows-arm64.exe` instead. The verified
firmware will be saved as `K223RGB_V10003.bin` in the current directory.

## Download

Download the archive for your platform from
[GitHub Releases](https://github.com/Belonit/k223fetch/releases) and unpack it.

Keep `sources.json` in the same directory as the executable:

```text
k223fetch/
├── k223fetch-windows-amd64.exe
└── sources.json
```

Release executable names are:

- Windows: `k223fetch-windows-amd64.exe`, `k223fetch-windows-arm64.exe`
- macOS: `k223fetch-darwin-amd64`, `k223fetch-darwin-arm64`
- Linux: `k223fetch-linux-amd64`, `k223fetch-linux-arm64`

## Usage

The examples below use the Linux amd64 filename. Substitute the executable
name for your platform. Download the firmware using the fastest available
source:

```sh
./k223fetch-linux-amd64
```

Show the configured sources:

```sh
./k223fetch-linux-amd64 list
```

Extract the firmware from an existing package:

```sh
./k223fetch-linux-amd64 extract PACKAGE
```

Choose a different output path:

```sh
./k223fetch-linux-amd64 -output firmware.bin
```

The utility verifies the extracted firmware before saving it and does not
overwrite an existing unrecognized file.

## Build

Build for the current platform:

```sh
CGO_ENABLED=0 go build .
```

Build all supported targets:

```sh
./build-release.sh
```

## License

`k223fetch` is distributed under the BSD 3-Clause License. See
[`LICENSE.md`](LICENSE.md) and
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
