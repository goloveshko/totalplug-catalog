# 📦 TotalPlug Community Catalog

[![Validate & Build](https://github.com/goloveshko/totalplug-catalog/actions/workflows/build.yml/badge.svg)](https://github.com/goloveshko/totalplug-catalog/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Catalog CDN](https://img.shields.io/badge/CDN-catalog.resolved.json-blue)](https://sergey.is-a.dev/totalplug-catalog/catalog.resolved.json)

An open-source, community-driven registry and update database for **Total Commander** plugins (`WCX`, `WLX`, `WFX`, `WDX`).

Used as the primary update and discovery source for the [TotalPlug](https://github.com/goloveshko/totalplug) manager.

---

## 🚀 Live Endpoints

The catalog is compiled and updated automatically every 6 hours via GitHub Actions:

- **Snapshot (Resolved Versions & Assets):**  
  `https://sergey.is-a.dev/totalplug-catalog/catalog.resolved.json`
- **Raw Master Catalog:**  
  `https://sergey.is-a.dev/totalplug-catalog/catalog.json`
- **Minified CDN Version:**  
  `https://sergey.is-a.dev/totalplug-catalog/catalog.min.json`

---

## 📁 Repository Structure

```text
totalplug-catalog/
├── plugins/
│   ├── wcx/                  # Packer plugins (archives, containers)
│   ├── wlx/                  # Lister plugins (viewers, highlighters)
│   ├── wfx/                  # File System plugins (FTP, Cloud, ADB)
│   └── wdx/                  # Content plugins (file attributes, metadata)
├── schemas/
│   └── plugin.schema.json    # JSON Schema for automated validation
└── scripts/
    └── builder/              # Go compiler & metadata resolver
```

---

## 🤝 How to Add or Update a Plugin

Adding a plugin takes less than 2 minutes:

1. **Fork** this repository.
2. Create a new JSON file inside `plugins/<type>/<plugin-id>.json` (e.g., `plugins/wlx/cudalister.json`).
3. Follow the schema format:

```json
{
  "$schema": "../../schemas/plugin.schema.json",
  "id": "cudalister",
  "name": "CudaLister",
  "type": "WLX",
  "description": "Syntax highlighting viewer plugin based on CudaText engine",
  "authors": ["Alexey-T"],
  "homepage": "https://github.com/Alexey-T/CudaLister",
  "license": "MPL-2.0",
  "match": {
    "aliases": ["cudalister", "wlx_cudalister"],
    "filenames": ["cudalister.wlx", "cudalister.wlx64"]
  },
  "source": {
    "type": "github_release",
    "repo": "Alexey-T/CudaLister",
    "asset_pattern": "wlx_cudalister.*\\.zip"
  }
}
```

4. Open a **Pull Request**. CI will validate your JSON automatically.
5. Once merged, it goes live in the global catalog!

---

## 📜 Supported Source Types

| Source Type      | Description                                                     | Required Fields                    |
| :--------------- | :-------------------------------------------------------------- | :--------------------------------- |
| `github_release` | Dynamically fetches latest release tag & assets from GitHub API | `repo`, `asset_pattern` (optional) |
| `direct_url`     | Fixed download link with static version                         | `download_url`, `version`          |
| `totalcmd_net`   | Links to existing totalcmd.net ID                               | `totalcmd_id`                      |

---

## 🙏 Credits & Acknowledgments

- [totalcmd.net](https://totalcmd.net/) — The original long-standing Total Commander plugin database.
- **Total Commander** is a registered trademark of Christian Ghisler / Ghisler Software GmbH.

---

## ⚖️ License

All catalog metadata is released under the [MIT License](LICENSE).
