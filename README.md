# MyWhoosh2Garmin

Überträgt deine [MyWhoosh](https://www.mywhoosh.com/)-Indoor-Fahrten nach [Garmin Connect](https://connect.garmin.com/) — inklusive **Training Effect**, **VO2max**, **Training Load** und **Training Status**.

Die Fahrten werden direkt aus deinem MyWhoosh-Konto geladen. Das Programm muss also nicht auf dem PC laufen, auf dem MyWhoosh installiert ist.

Es gibt zwei Varianten:

| Variante | Für wen | Bezug |
|---|---|---|
| **Windows-App** (Desktop-GUI) | Du willst gelegentlich per Klick synchronisieren | [Releases](../../releases) → `mywhoosh2garmin-windows-amd64.exe` |
| **Docker** (Web-App + Webhooks + Auto-Sync) | Du hast einen Server/NAS/Raspberry Pi und willst, dass es von selbst läuft | Fertiges Image aus der GitHub Container Registry |

## Warum überhaupt?

MyWhoosh exportiert FIT-Dateien, die Garmin nicht vollständig verarbeitet:

| Problem | Lösung |
|---|---|
| Durchschnittsleistung / -puls / -kadenz fehlen | werden aus den Aufzeichnungen berechnet |
| Falsche Temperaturwerte in jedem Datensatz | werden entfernt |
| Unbekanntes Gerät → Garmin ignoriert Training Effect & VO2max | Gerät wird als Garmin Fenix 6S Pro eingetragen |

## Windows-App

1. `mywhoosh2garmin-windows-amd64.exe` von der [Releases](../../releases)-Seite laden und starten (keine Installation nötig).
2. MyWhoosh- und Garmin-Zugangsdaten eingeben. Passwörter werden nur für die erste Anmeldung gebraucht und **nicht gespeichert**; danach wird ein Session-Token unter `%USERPROFILE%\.mywhoosh2garmin\` wiederverwendet (Garmin: bis zu einem Jahr).
3. **Fetch Activities** lädt die Fahrten der letzten Tage (Standard 10).
4. Pro Fahrt **Upload**, oder **Upload All** für alle neuen.

### Fahrt schon auf Garmin? Dauerhaft als hochgeladen markieren

Wurde eine Fahrt bereits auf anderem Weg (z. B. von Hand oder mit einem anderen Tool) zu Garmin übertragen, erkennt das Programm das nicht immer — Garmin sieht die korrigierte Datei dann nicht als Duplikat. So verhinderst du eine doppelte Aktivität:

- **Already on Garmin** neben der Fahrt: merkt sie sich dauerhaft als hochgeladen. Sie wird nie wieder hochgeladen, auch nicht von „Upload All“ oder dem Auto-Sync.
- **Mark all open as uploaded**: markiert auf einmal alle noch offenen Fahrten der Liste.
- **Undo**: nimmt die Markierung wieder weg.

Die Markierungen stehen in `synced.json` im Datenordner (Windows: `%USERPROFILE%\.mywhoosh2garmin\`, Docker: Volume `/data`) und überleben Neustarts und Updates.

## Docker

Das Image wird automatisch gebaut und in der GitHub Container Registry bereitgestellt (`linux/amd64` und `linux/arm64`). Du baust nichts lokal — du gibst in der `docker-compose.yml` nur an, welche Version laufen soll.

### Starten

```bash
# Nur diese zwei Dateien werden gebraucht:
curl -O https://raw.githubusercontent.com/HauZ22/mywoosh2garmin/main/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/HauZ22/mywoosh2garmin/main/.env.example

docker compose up -d
# Web-App: http://localhost:8080
```

Die Zugangsdaten kannst du in `.env` eintragen oder bequem in der Web-App eingeben.

### Version wählen

In `.env`:

```ini
IMAGE_TAG=latest   # zuletzt veröffentlichte Version (Standard)
IMAGE_TAG=1.2.3    # genau diese Version — empfohlen für ein stabiles Setup
IMAGE_TAG=edge     # aktueller Stand des main-Branches (Vorabversion)
```

### Aktualisieren

```bash
docker compose pull && docker compose up -d
```

Tokens, Konfiguration und die Sync-Historie liegen im Volume `fittogarmin-data` und bleiben dabei erhalten.

> **Einmalig nötig (Repo-Besitzer):** GitHub legt neue Pakete als *privat* an. Unter *GitHub → Profil → Packages → mywhoosh2garmin → Package settings → Change visibility* auf **Public** stellen, sonst kann `docker compose pull` das Image nicht ohne Login laden.

### Lokal aus dem Quellcode bauen (optional)

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

### Zugriffsschutz

Die Web-App kann Zugangsdaten verwalten und Uploads auslösen. Setze deshalb in `.env` ein `AUTH_PASSWORD` (Benutzer: `AUTH_USER`, Standard `admin`), sobald der Port nicht nur lokal erreichbar ist — oder binde ihn in der Compose-Datei an `127.0.0.1:8080:8080`. Webhook-Aufrufe brauchen dann `curl -u admin:passwort …`. Für Zugriff aus dem Internet gehört ein Reverse-Proxy mit HTTPS davor.

### Web-App

- **Aktivitäten laden**: Fahrten der letzten Tage mit Status *Neu / Hochgeladen / Auf Garmin vorhanden / Manuell markiert*.
- Pro Fahrt **Upload** oder **Schon auf Garmin** (dauerhaft als hochgeladen merken); bei bereits markierten Fahrten **Markierung entfernen**.
- **Alle offenen als hochgeladen markieren** und **Alle neuen synchronisieren**.
- **Auto-Sync**: Intervall in Minuten (0 = aus). Beim Einschalten werden ältere Fahrten nie automatisch hochgeladen, sondern nur als „übersprungen“ gemerkt.
- **Datei hochladen**: FIT oder TCX (z. B. vom [JOIN Cycling](https://www.join.cc/) Workout-Player) per Drag & Drop.

### Endpunkte

| Methode | Pfad | Beschreibung |
|---|---|---|
| GET | `/` | Web-App |
| GET | `/healthz` | Health-Check (immer ohne Login) |
| GET | `/api/status` | Status, letzte Läufe, Log |
| POST | `/api/config` | Zugangsdaten / Zeitfenster / Auto-Sync speichern (JSON) |
| GET | `/api/activities?days=N` | Fahrten mit Sync-Status |
| POST | `/api/activities/mark` | Fahrten markieren: `{"ids":["…"],"synced":true}` (`false` = Markierung entfernen) |
| POST | `/api/sync` · `/api/webhook/mywhoosh` | Sync aller neuen Fahrten starten |
| POST | `/api/sync/activity` | Eine Fahrt hochladen: `{"id":"…"}` |
| POST | `/api/upload` · `/api/webhook/upload` | Datei (`.fit`/`.tcx`) patchen und hochladen |

Jobs laufen asynchron: Die Endpunkte antworten sofort mit `202`; der Fortschritt steht in `/api/status`. Läuft schon ein Job, kommt `409`.

```bash
# MyWhoosh-Sync per Cron anstoßen
curl -X POST http://localhost:8080/api/webhook/mywhoosh

# JOIN-TCX direkt senden
curl -X POST http://localhost:8080/api/webhook/upload -F "file=@join_workout.tcx"

# oder als rohen Body mit Dateinamen-Header
curl -X POST http://localhost:8080/api/webhook/upload \
  -H "X-Filename: join_workout.tcx" --data-binary @join_workout.tcx
```

### Umgebungsvariablen

| Variable | Default | Beschreibung |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Listen-Adresse |
| `DATA_DIR` | `./data` (Container: `/data`) | Zustandsverzeichnis |
| `GARMIN_EMAIL` / `GARMIN_PASSWORD` | — | Garmin-Konto (überschreibt Web-Konfig) |
| `MYWHOOSH_EMAIL` / `MYWHOOSH_PASSWORD` | — | MyWhoosh-Konto |
| `SYNC_DAYS` | `10` | Sichtfenster in Tagen |
| `AUTH_USER` / `AUTH_PASSWORD` | `admin` / — | Basic-Auth; ohne Passwort kein Schutz |

### JOIN Cycling (TCX)

Der JOIN Workout-Player exportiert TCX-Dateien, die Garmin Connect nicht direkt annimmt. Der Server entfernt den JOIN-Author-Block und trägt ein Garmin-Gerät (Fenix 6S Pro) als `<Creator>` ein. Die Trackdaten (Leistung, Puls, Kadenz, Geschwindigkeit) bleiben unverändert.

## Einschränkungen

- **Kein MFA**: Ist bei deinem Garmin-Konto die Zwei-Faktor-Anmeldung aktiv, schlägt der Login fehl.
- Der Garmin-Login nutzt inoffizielle Schnittstellen und kann sich ohne Vorwarnung ändern.

## Entwicklung

```bash
# Tests (kein C-Compiler nötig)
go test ./internal/... ./garmin/... ./mywhoosh/... ./cmd/...

# Server (ohne CGO)
CGO_ENABLED=0 go build -o mywhoosh2garmin-server ./cmd/server

# Desktop-GUI (braucht GCC; unter Windows z. B. MSYS2/MinGW)
go build -o mywhoosh2garmin.exe .
```

### Neue Version veröffentlichen

```bash
git tag v1.2.0 && git push origin v1.2.0
```

Das löst zwei Workflows aus:

- `release.yml` baut die Windows-App und hängt sie an das GitHub-Release.
- `docker.yml` veröffentlicht das Image als `ghcr.io/hauz22/mywhoosh2garmin:1.2.0`, `:1.2`, `:1` und `:latest`.

Jeder Push auf `main` aktualisiert zusätzlich das Vorabimage `:edge`.

## Credits

- [garth](https://github.com/matin/garth) — Referenz für die Garmin-SSO-Anmeldung
- [muktihari/fit](https://github.com/muktihari/fit) — FIT SDK für Go
- [Fyne](https://fyne.io/) — GUI-Toolkit

## Lizenz

GPLv3 — siehe [LICENSE](LICENSE).
