#!/usr/bin/env python3
"""Transcribe a YouTube video for gary agents: `gary transcribe <url>`.

1. YouTube's own captions (manual first, then auto-generated), English preferred.
2. No captions: download the audio and run faster-whisper locally (CPU, int8).
Prints a header (title, url, source) and the plain text. Cached per video id in
~/.cache/gary-transcripts/<id>.txt, so a second ask is instant.
"""
import argparse, glob, html, json, os, re, subprocess, sys, tempfile

VENV = os.path.dirname(os.path.abspath(__file__))
YTDLP = os.path.join(VENV, "bin", "yt-dlp")
CACHE = os.path.expanduser("~/.cache/gary-transcripts")
ID_RE = re.compile(r"(?:v=|youtu\.be/|shorts/|embed/|live/)([A-Za-z0-9_-]{11})")


def video_id(url):
    m = ID_RE.search(url)
    if m:
        return m.group(1)
    if re.fullmatch(r"[A-Za-z0-9_-]{11}", url):
        return url
    sys.exit(f"gary transcribe: not a YouTube link: {url}")


def run(args, **kw):
    return subprocess.run(args, capture_output=True, text=True, **kw)


def vtt_text(path):
    """Plain text from a VTT file; auto-captions repeat each line as it scrolls, so drop repeats."""
    out, last = [], None
    for line in open(path, encoding="utf-8", errors="replace"):
        line = line.strip()
        if not line or line == "WEBVTT" or "-->" in line or re.match(r"^(Kind|Language|NOTE)\b", line) or line.isdigit():
            continue
        line = html.unescape(re.sub(r"<[^>]+>", "", line)).strip()
        if line and line != last:
            out.append(line)
            last = line
    # join, then collapse the overlap auto-captions leave between consecutive cues
    text = " ".join(out)
    return re.sub(r"\s+", " ", text).strip()


def meta(url):
    r = run([YTDLP, "--skip-download", "--no-warnings", "--print", "%(title)s\t%(duration)s\t%(channel)s", url], timeout=120)
    if r.returncode == 0 and r.stdout.strip():
        title, dur, chan = (r.stdout.strip().split("\t") + ["", "", ""])[:3]
        return title, dur, chan
    return "", "", ""


def captions(url, tmp):
    for flags in (["--write-subs"], ["--write-auto-subs"]):
        run([YTDLP, "--skip-download", "--no-warnings", "--ignore-errors", *flags,
             "--sub-langs", "en,en-US,en-GB,en-orig", "--sub-format", "vtt",
             "-o", os.path.join(tmp, "s.%(ext)s"), url], timeout=180)
        files = sorted(glob.glob(os.path.join(tmp, "s*.vtt")))
        for f in files:
            t = vtt_text(f)
            if len(t) > 20:
                return t, "YouTube captions" if flags == ["--write-subs"] else "YouTube auto-captions"
    return None, None


def whisper(url, tmp, model):
    r = run([YTDLP, "--no-warnings", "-f", "bestaudio/best", "-x", "--audio-format", "m4a",
             "-o", os.path.join(tmp, "a.%(ext)s"), url], timeout=1800)
    audio = sorted(glob.glob(os.path.join(tmp, "a.*")))
    if not audio:
        sys.exit("gary transcribe: no captions, and the audio download failed:\n" + (r.stderr or r.stdout)[-800:])
    # decode with the system ffmpeg to 16 kHz mono float32 (faster-whisper's own PyAV decoder breaks on
    # PyAV 19), then hand it the samples
    import numpy as np
    from faster_whisper import WhisperModel
    pcm = subprocess.run(["ffmpeg", "-nostdin", "-loglevel", "error", "-i", audio[0], "-f", "f32le",
                          "-ac", "1", "-ar", "16000", "-"], capture_output=True, check=True).stdout
    samples = np.frombuffer(pcm, dtype=np.float32)
    m = WhisperModel(model, device="cpu", compute_type="int8")
    segs, info = m.transcribe(samples, vad_filter=True)
    text = " ".join(s.text.strip() for s in segs)
    return text, f"local speech-to-text (faster-whisper {model}, language {info.language})"


def main():
    p = argparse.ArgumentParser(prog="gary transcribe")
    p.add_argument("url")
    p.add_argument("--model", default="small", help="faster-whisper model when there are no captions")
    p.add_argument("--force", action="store_true", help="ignore the cache")
    p.add_argument("--whisper", action="store_true", help="skip captions, always run speech-to-text")
    a = p.parse_args()
    vid = video_id(a.url)
    url = f"https://www.youtube.com/watch?v={vid}"
    os.makedirs(CACHE, exist_ok=True)
    cached = os.path.join(CACHE, vid + ".txt")
    if os.path.exists(cached) and not a.force:
        sys.stdout.write(open(cached, encoding="utf-8").read())
        return
    title, dur, chan = meta(url)
    with tempfile.TemporaryDirectory() as tmp:
        text, source = (None, None) if a.whisper else captions(url, tmp)
        if not text:
            text, source = whisper(url, tmp, a.model)
    mins = ""
    try:
        mins = f"{int(float(dur)) // 60}:{int(float(dur)) % 60:02d}"
    except ValueError:
        pass
    head = f"# {title or vid}\n{url}" + (f" · {chan}" if chan else "") + (f" · {mins}" if mins else "") + f"\nsource: {source}\n\n"
    out = head + text + "\n"
    open(cached, "w", encoding="utf-8").write(out)
    sys.stdout.write(out)


if __name__ == "__main__":
    main()
