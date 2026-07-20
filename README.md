# Ferry

A friendly USB creator for [FyshOS](https://github.com/FyshOS/fyshos). Ferry downloads
the latest FyshOS image and writes it to a USB stick, verifying the result and confirming output.

![Ferry choosing an image](img/screenshot.png)

## Features

- **Fetches the latest release** directly from the FyshOS GitHub releases, matched to
  your architecture (amd64, arm64 or i386).
- **Reuses what you already downloaded** — images are cached, so a second stick costs
  no bandwidth.
- **Writes any ISO**, not just FyshOS, if you already have one on disk.
- **Verifies after writing.** Ferry reads the device back and compares its SHA-256
  against the source image, so a bad stick is reported rather than discovered at boot.

## Installing

Ferry is a Go application built with [Fyne](https://fyne.io).

```
go install fyne.io/tools/cmd/fyne@latest
fyne install github.com/fyshos/ferry@latest
```

Or build from a checkout:

```
git clone https://github.com/fyshos/ferry.git
cd ferry
fyne install
```

## Requirements

Writing to USB media is currently **Linux only** — Ferry builds on other platforms but
will report that device writing is unsupported.

Ferry shells out to a few standard utilities, all present on a typical desktop install:

| Tool | Used for |
| --- | --- |
| `lsblk` | listing removable devices |
| `pkexec` | requesting administrator rights to write |
| `dd`, `sync`, `blockdev` | writing and flushing the image |
| `eject` or `udisksctl` | powering the device down when finished |

`pkexec` needs a polkit authentication agent running in your desktop session. Most
desktops start one automatically; if the authorisation prompt never appears, that agent
is the thing to check.

## How it works

Ferry is a four step wizard:

1. **Architecture** — pick the CPU the target machine uses. Defaults to your own.
2. **Image** — download the latest FyshOS, pick one you already have, or open any `.iso`.
3. **Device** — choose from the removable drives Ferry found.
4. **Confirm & write** — a last look before anything is overwritten, then write and verify.

Downloads land in the application cache directory and are written to a `.part` file that
is only renamed into place on success, so an interrupted download never masquerades as a
complete image.

The write itself runs as a single privileged shell script under `pkexec`: it unmounts any
mounted partitions, `dd`s the image across, flushes the device's buffer cache, reads back
exactly as many bytes as the image is long, and compares checksums. Progress is streamed
back to the UI as the script runs.

## Safety

Writing an image **erases the entire target device**. Ferry only lists removable,
hotpluggable or USB-attached drives — your system disk will not appear — and the
confirmation step shows the device's model, path and size. It still cannot know which
stick matters to you, so check those details before you continue.

Once writing starts it cannot be cancelled: there is no safe midpoint to stop a
half-written device at. Leave the stick plugged in until Ferry says it is done.
