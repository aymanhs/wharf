# wharf

A minimal, keyboard-first Docker TUI for people who move fast.

If your daily flow is Docker PS, Docker exec, restart, delete, logs, and image cleanup, wharf keeps all of it in one lightweight terminal UI.

## Why wharf

- Single binary
- SSH-friendly
- No config
- No plugin system
- No dashboards, no noise
- Fast focus-and-act workflow

## Core idea

Pick target, press key, done.

## Features

- Container view
  - Running-only or all containers
  - Restart, delete (with confirmation)
  - Exec bash
  - Follow logs
- Image view
  - List images with short ID, name, size, age
  - Select multiple images
  - Delete one or bulk delete selected
  - One-key prune with confirmation
- Responsive fullscreen table UI
- Safe destructive actions via confirm prompts

## Keybindings

### Global

- j / k or arrows: move
- i or Tab: switch containers/images
- g or Ctrl+r: refresh
- q: quit

### Containers mode

- a: toggle running/all
- r: restart selected container
- d: delete selected container (confirm)
- x: exec bash in selected container
- l: follow logs for selected container

### Images mode

- Space: toggle image selection
- d: delete selected image (confirm)
- D: delete selected images (confirm)
- p: prune dangling images (confirm)

## Install

### Build locally

```bash
go build -o wharf .
```

### Run

```bash
./wharf
```

## Philosophy

Wharf is not trying to be Docker Desktop in your terminal.

It is intentionally opinionated:

- minimal surface area
- keyboard-first UX
- fast operations for real-world Docker maintenance

## Status

Actively evolving as a focused V1 tool.
