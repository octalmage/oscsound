# oscsound

A tiny soundboard that plays local audio when specific VRChat avatar parameters update.

VRChat caps active avatar audio sources at 3, which is rough for anything with
more than a couple SFX. This sidesteps that: VRChat sends OSC, this app plays
the sound through your PC's audio output. OBS picks it up like any other game
audio. Other players in VRChat won't hear it without extra routing (like VB-Cable
into your mic) but useful for streaming.

## Usage

1. In VRChat: enable OSC (Action Menu → Options → OSC → Enabled).
2. Run oscsound. It registers itself with VRChat via OSCQuery.
3. Add a sound: give it a name, type the avatar parameter name (without the
   `/avatar/parameters/` prefix), pick a `.wav`, `.mp3`, or `.ogg` file, choose a type.
4. Toggle the parameter on your avatar then the matching row flashes and the sound plays.

### Trigger types

- **one-shot**: plays once on each rising edge. Stomps, hits, bursts.
- **loop**: starts on true, loops forever, stops on false. Rumble, ambience.

Multiple sounds (one-shots, loops, or both) play and mix together: there's no
limit.

### Packs

- **Export**: writes a single `.zip` containing every sound file and a `manifest.json` linking each to its avatar parameter and type.
- **Import**: hit Import, pick the soundpack and you're configured.

App config is saved to your OS config dir (`~/Library/Application Support/oscsound/`
on macOS, `%AppData%\oscsound\` on Windows). Imported soundpack contents live under
`packs/` in the same directory.

## Dev

```
wails dev
```

To test without VRChat, send a bool OSC message to `127.0.0.1:port` at `/avatar/parameters/<YourParam>` using any OSC client. You'll have to find the OSCQuery port from the oscsound UI. I use [sendosc](https://github.com/yoggy/sendosc) for testing.

## Build

```
wails build                                  # current platform
wails build -platform windows/amd64          # cross-build for Windows (needs docker)
```

## License

GPL-3.0
