"""Cut clips out of one audio file, so a detection can be heard on its own.

The Go server runs this after analyze.py (backend/internal/birdnet), once per
analyzed file, while the file is still on local disk.

    python clip.py < spec.json

stdin is one JSON object: the source file, and the clips to cut from it in
seconds into the recording.

    {"source": "/tmp/a.wav",
     "clips": [{"startSec": 731, "endSec": 739, "path": "/tmp/clip-0.flac"}]}

Each clip is written as mono 16-bit FLAC at the source's sample rate: lossless,
under half the size of the same WAV, and playable and decodable in every browser
the app supports. stdout carries exactly one JSON object: the source's length
and sample rate, and each clip as cut, clamped to the recording.

    {"durationSec": 3600.0, "sampleRate": 48000,
     "clips": [{"path": "/tmp/clip-0.flac", "startSec": 731, "endSec": 739}]}

Anything that goes wrong exits non-zero with the reason on stderr. soundfile is
what birdnet reads audio with, so any file BirdNET could analyze can be cut.
"""

import json
import sys

import soundfile


def main() -> int:
    spec = json.load(sys.stdin)
    cut = []
    with soundfile.SoundFile(spec["source"]) as src:
        rate, frames = src.samplerate, src.frames
        for clip in spec.get("clips", []):
            start = min(max(0, round(clip["startSec"] * rate)), frames)
            end = min(max(start, round(clip["endSec"] * rate)), frames)
            src.seek(start)
            audio = src.read(end - start, dtype="float32", always_2d=True)
            # Mono is what BirdNET listened to, and half the size of stereo.
            # FLAC is integer-only, so PCM_16 is the subtype either way.
            soundfile.write(clip["path"], audio.mean(axis=1), rate, format="FLAC", subtype="PCM_16")
            cut.append({
                "path": clip["path"],
                "startSec": round(start / rate, 3),
                "endSec": round((start + len(audio)) / rate, 3),
            })

    json.dump({"durationSec": round(frames / rate, 3), "sampleRate": rate, "clips": cut}, sys.stdout)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
