# MyWhoosh2Garmin

A single-executable GUI app that syncs your [MyWhoosh](https://www.mywhoosh.com/) indoor cycling activities to [Garmin Connect](https://connect.garmin.com/) — with full training effect, VO2max, and performance stats support.

No need to run this on the same PC as MyWhoosh — the app downloads activities directly from your MyWhoosh account.


## Why?

MyWhoosh exports FIT files, but they have issues that prevent Garmin from fully processing them:

- **Missing averages** — average power, heart rate, and cadence are not set in the session data
- **Bogus temperature** — every record contains a fake temperature reading
- **Unknown device** — Garmin ignores training effect and VO2max from unknown manufacturers

MyWhoosh2Garmin fixes all of this automatically:

| Problem | Fix |
|---|---|
| Missing avg power / HR / cadence | Calculated from ride records |
| Fake temperature data | Stripped from all records |
| MyWhoosh device identity | Spoofed to Garmin Fenix 6S Pro |

The result: your indoor rides show up on Garmin Connect just like a native Garmin recording, complete with **Training Effect**, **VO2max updates**, **Training Load**, and **Training Status**.

## Download

Grab the latest release for your platform from the [**Releases**](../../releases) page:

| Platform | File |
|---|---|
| Windows | `mywhoosh2garmin-windows-amd64.exe` |
| Linux | `mywhoosh2garmin-linux-amd64` |

No installation needed — just download and run.

## How to Use

### 1. Enter MyWhoosh credentials

Enter your **MyWhoosh email** and **password**. These are used to log in to the MyWhoosh API and fetch your activity list.

After the first login, the session token is cached locally (`~/.mywhoosh2garmin/`) so you won't need to enter your password again unless the token expires.

### 2. Enter Garmin credentials

Enter your **Garmin Connect email** and **password**. These are only sent directly to Garmin's SSO servers — never stored or sent anywhere else.

After the first login, a session token is cached locally (`~/.mywhoosh2garmin/`) and reused for up to a year. You won't need to enter your password again unless the token expires.

### 3. Fetch Activities

Click **📋 Fetch Activities (last 10 days)** and the app will:

1. Log in to your MyWhoosh account (or resume a cached session)
2. Fetch your activities from the last 10 days
3. Display them in a list showing date, title, distance, duration, power, and heart rate — with an upload button next to each one

### 4. Upload to Garmin

You can either:

- Click **⬆ Upload** next to individual activities to upload them one by one
- Click **⬆ Upload All to Garmin** to upload all unsynced activities at once

For each activity, the app will:

1. Download the FIT file from MyWhoosh
2. Fix averages, strip temperature, spoof device identity
3. Upload to Garmin Connect
4. Mark the activity as synced so it won't be uploaded again

## Building from Source

### Prerequisites

- Go 1.24+
- GCC (for Fyne/CGO)
- Windows cross-compile from Linux: `mingw-w64`, `libgl-dev`, `xorg-dev`, `libxxf86vm-dev`

### Build

```bash
# Desktop GUI
go build -o mywhoosh2garmin .

# Headless web server / webhook daemon (no CGO needed)
CGO_ENABLED=0 go build -o fittogarmin-server ./cmd/server

# Both platforms (requires mingw-w64)
./build.sh
# Output in dist/
```

### Run tests

```bash
# All non-GUI packages (no C-compiler needed)
go test ./internal/... ./garmin/... ./mywhoosh/... ./cmd/...
```

## Docker (Web-App + Webhook Server)

Der Server (`./cmd/server`) stellt die gleiche Sync-Logik als Web-App und
Headless-Webhook bereit — ohne Desktop/Display. Er holt MyWhoosh-Aktivitäten,
patcht FIT-Dateien und nimmt zusätzlich gepatchte **TCX-Dateien** (z. B. vom
[JOIN Cycling](https://www.join.cc/) Workout-Player, der TCX per E-Mail
verschickt) für den Upload nach Garmin Connect entgegen.

### Starten

```bash
cp .env.example .env      # Zugangsdaten eintragen
docker compose up -d
# Web-App: http://localhost:8080
```

Alternativ direkt mit `.env`-Angaben:

```bash
docker run -d --name fittogarmin -p 8080:8080 \
  -e GARMIN_EMAIL=... -e GARMIN_PASSWORD=... \
  -e MYWHOOSH_EMAIL=... -e MYWHOOSH_PASSWORD=... \
  -v fittogarmin-data:/data fittogarmin
```

Der Datenordner `/data` (Volume) persistiert Tokens (Garmin/MyWhoosh),
`config.json` und `synced.json`. Garmin/MyWhoosh-Passwörter werden **nur zur
erstmaligen Anmeldung** benötigt — danach werden die Session-Tokens (> 1 Jahr
gültig) wiederverwendet. Die Konten lassen sich auch bequem über die Web-App
konfigurieren.

### Endpunkte

| Methode | Pfad | Beschreibung |
|---|---|---|
| GET | `/` | Web-App (Konfig, MyWhoosh-Sync, Datei-Upload, Log) |
| GET | `/api/status` | Status, letzte Läufe, Log-Tail |
| POST | `/api/config` | Zugangsdaten / Sichtfenster (Tage) speichern (JSON) |
| POST | `/api/sync` | MyWhoosh-Sync jetzt starten |
| POST | `/api/upload` | Datei (`.fit`/`.tcx`) hochladen → patch → Garmin |
| POST | `/api/webhook/mywhoosh` | Headless-Trigger für den MyWhoosh-Sync |
| POST | `/api/webhook/upload` | Headless-Dateiempfänger (z. B. JOIN-TCX) |

### Webhook-Beispiele

```bash
# MyWhoosh-Sync headless anstoßen (z. B. mit einem cron-Job)
curl -X POST http://localhost:8080/api/webhook/mywhoosh

# JOIN-TCX direkt aus einem Skript senden (nach Mail-Export)
curl -X POST http://localhost:8080/api/webhook/upload \
  -F "file=@join_workout_....tcx"

# Oder roher Body mit Dateinamen-Header
curl -X POST http://localhost:8080/api/webhook/upload \
  -H "X-Filename: join_workout.tcx" \
  --data-binary @join_workout_....tcx
```

Die Jobs laufen asynchron: die Endpunkte antworten sofort mit `202`, der
Fortschritt erscheint in `/api/status` (und der Web-App). Während ein Sync
läuft, antworten weitere Jobs mit `409`. Es gibt bewusst keine Authentifizierung
— halte den Port hinter NAT/firewall oder binde den Server nur an `127.0.0.1`
(`HTTP_ADDR=127.0.0.1:8080`), wenn du Zugangsdaten drauflegst.

### Umgebungsvariablen

| Variable | Default | Beschreibung |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Listen-Adresse |
| `DATA_DIR` | `./data` | Zustandsverzeichnis (im Container `/data`) |
| `GARMIN_EMAIL` / `GARMIN_PASSWORD` | — | Garmin-Konto (überschreibt Web-Konfig) |
| `MYWHOOSH_EMAIL` / `MYWHOOSH_PASSWORD` | — | MyWhoosh-Konto |
| `SYNC_DAYS` | `10` | MyWhoosh-Sichtfenster (Tage) |

### JOIN Cycling (TCX)

Der JOIN Workout-Player verschickt das Training als TCX-Datei (per E-Mail) —
Garmin unterstützt das direkte Hochladen nicht. Der Server übernimmt das:
Er entfernt den JOIN-Author-Block und versieht die Activity mit einem Garmin
Device-`<Creator>` (Fenix 6S Pro) — dieselbe Spoofing-Logik wie beim
MyWhoosh-FIT -, sodass Garmin Connect die Einheit vollständig verarbeitet. Die
Trackdaten (Power, HR, Cadence, Speed) bleiben byte-genau erhalten.

## How It Works

```
  ┌─────────────────────┐
  │  MyWhoosh Web API   │
  │  Login + List       │
  │  Download FIT file  │
  └─────────┬───────────┘
            │
            ▼
  ┌─────────────────────┐
  │  Decode FIT (V2)    │
  │  Fix session avgs   │
  │  Strip temperature  │
  │  Spoof → Fenix 6S   │
  │  Encode FIT (V2)    │
  └─────────┬───────────┘
            │
            ▼
  ┌─────────────────────┐
  │  Garmin SSO Login   │
  │  OAuth1 → OAuth2    │
  │  Upload FIT file    │
  └─────────────────────┘
            │
            ▼
     Garmin Connect
  (Training Effect ✓)
  (VO2max ✓)
  (Training Load ✓)
```

## Credits

- [garth](https://github.com/matin/garth) by matin — Garmin SSO authentication reference
- [muktihari/fit](https://github.com/muktihari/fit) — FIT SDK for Go
- [Fyne](https://fyne.io/) — cross-platform GUI toolkit

## License

GPLv3 — see [LICENSE](LICENSE).
