#!/usr/bin/env python3
"""Let a gary agent "watch" a YouTube video: `gary watch <url>`.

Downloads the video at low resolution, takes a frame at every scene change plus one every
--every seconds (so static talks are covered too), tiles them into contact sheets of 3 x 3
stamped with their time, and writes a transcript with [mm:ss] times. Prints where everything
is: the agent opens the sheets with its Read tool and reads the transcript alongside.
Output: ~/.cache/gary-watch/<id>/ (sheet_01.jpg ..., frames/, transcript.txt). Cached.
"""
import argparse, glob, html, json, os, re, shutil, subprocess, sys, tempfile

VENV = os.path.dirname(os.path.abspath(__file__))
YTDLP = os.path.join(VENV, "bin", "yt-dlp")
CACHE = os.path.expanduser("~/.cache/gary-watch")
ID_RE = re.compile(r"(?:v=|youtu\.be/|shorts/|embed/|live/)([A-Za-z0-9_-]{11})")
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"


def video_id(url):
    m = ID_RE.search(url)
    if m:
        return m.group(1)
    if re.fullmatch(r"[A-Za-z0-9_-]{11}", url):
        return url
    sys.exit(f"gary watch: not a YouTube link: {url}")


def clock(t):
    t = int(t)
    return f"{t // 3600}:{t // 60 % 60:02d}:{t % 60:02d}" if t >= 3600 else f"{t // 60}:{t % 60:02d}"


def parse_ts(s):
    h, m, rest = (["0"] + s.split(":"))[-3:]
    return int(h) * 3600 + int(m) * 60 + float(rest.replace(",", "."))


def vtt_cues(path):
    """(start seconds, text) per cue, auto-caption scroll repeats dropped."""
    cues, last, start, buf = [], None, None, []
    def flush():
        nonlocal buf, last
        for line in buf:
            line = html.unescape(re.sub(r"<[^>]+>", "", line)).strip()
            if line and line != last:
                cues.append((start, line))
                last = line
        buf = []
    for raw in open(path, encoding="utf-8", errors="replace"):
        line = raw.strip()
        if "-->" in line:
            flush()
            start = parse_ts(line.split("-->")[0].strip().split(" ")[0])
        elif line and start is not None and not line.isdigit():
            buf.append(line)
    flush()
    return cues


def captions(url, tmp):
    for flags in (["--write-subs"], ["--write-auto-subs"]):
        subprocess.run([YTDLP, "--skip-download", "--no-warnings", "--ignore-errors", *flags,
                        "--sub-langs", "en,en-US,en-GB,en-orig", "--sub-format", "vtt",
                        "-o", os.path.join(tmp, "s.%(ext)s"), url], capture_output=True, timeout=180)
        for f in sorted(glob.glob(os.path.join(tmp, "s*.vtt"))):
            cues = vtt_cues(f)
            if cues:
                return cues, "YouTube captions" if flags == ["--write-subs"] else "YouTube auto-captions"
    return None, None


def whisper_cues(video, model):
    import numpy as np
    from faster_whisper import WhisperModel
    pcm = subprocess.run(["ffmpeg", "-nostdin", "-loglevel", "error", "-i", video, "-f", "f32le",
                          "-ac", "1", "-ar", "16000", "-"], capture_output=True, check=True).stdout
    m = WhisperModel(model, device="cpu", compute_type="int8")
    segs, info = m.transcribe(np.frombuffer(pcm, dtype=np.float32), vad_filter=True)
    return [(s.start, s.text.strip()) for s in segs], f"local speech-to-text (faster-whisper {model})"


def frames(video, out, every, scene, max_frames, start, end):
    """Scene changes plus one every `every` s; returns [(t, path)]. Thins to max_frames."""
    os.makedirs(out, exist_ok=True)
    sel = f"gt(scene\\,{scene})+isnan(prev_selected_t)+gte(t-prev_selected_t\\,{every})"
    args = ["ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "info"]
    if start:
        args += ["-ss", str(start)]
    args += ["-i", video]
    if end:
        args += ["-to", str(end - (start or 0))]
    args += ["-vf", f"select='{sel}',scale=480:-2,showinfo", "-vsync", "vfr", "-q:v", "4",
             os.path.join(out, "f_%04d.jpg")]
    r = subprocess.run(args, capture_output=True, text=True)
    times = [float(m.group(1)) + (start or 0) for m in re.finditer(r"pts_time:([0-9.]+)", r.stderr)]
    files = sorted(glob.glob(os.path.join(out, "f_*.jpg")))
    pairs = list(zip(times, files))
    if len(pairs) > max_frames:  # keep an even spread
        step = len(pairs) / max_frames
        keep = {int(i * step) for i in range(max_frames)}
        for i, (_, f) in enumerate(pairs):
            if i not in keep:
                os.remove(f)
        pairs = [p for i, p in enumerate(pairs) if i in keep]
    return pairs


def sheets(pairs, out, cols=3, rows=3):
    from PIL import Image, ImageDraw, ImageFont
    font = ImageFont.truetype(FONT, 22) if os.path.exists(FONT) else ImageFont.load_default()
    made, per = [], cols * rows
    for n in range(0, len(pairs), per):
        group = pairs[n:n + per]
        ims = [Image.open(f).convert("RGB") for _, f in group]
        w, h = ims[0].size
        sheet = Image.new("RGB", (cols * w, rows * h), (16, 16, 20))
        d = ImageDraw.Draw(sheet)
        for i, ((t, _), im) in enumerate(zip(group, ims)):
            x, y = (i % cols) * w, (i // cols) * h
            sheet.paste(im.resize((w, h)), (x, y))
            label = clock(t)
            d.rectangle([x, y, x + 12 * len(label) + 16, y + 30], fill=(0, 0, 0))
            d.text((x + 8, y + 3), label, fill=(255, 230, 120), font=font)
        path = os.path.join(out, f"sheet_{n // per + 1:02d}.jpg")
        sheet.save(path, quality=80)
        made.append((group[0][0], group[-1][0], path))
    return made


def main():
    p = argparse.ArgumentParser(prog="gary watch")
    p.add_argument("url")
    p.add_argument("--every", type=float, default=None,
                   help="a frame at least this often, seconds (default: about 60 frames, 1-10 s apart)")
    p.add_argument("--scene", type=float, default=0.3, help="scene-change threshold 0-1 (lower = more frames)")
    p.add_argument("--max-frames", type=int, default=90)
    p.add_argument("--from", dest="start", type=str, help="start at mm:ss")
    p.add_argument("--to", dest="end", type=str, help="stop at mm:ss")
    p.add_argument("--model", default="small", help="faster-whisper model when there are no captions")
    p.add_argument("--force", action="store_true")
    a = p.parse_args()
    vid = video_id(a.url)
    url = f"https://www.youtube.com/watch?v={vid}"
    start = parse_ts(a.start) if a.start else None
    end = parse_ts(a.end) if a.end else None
    tag = vid + (f"_{int(start or 0)}-{int(end) if end else 'end'}" if (start or end) else "")
    out = os.path.join(CACHE, tag)
    index = os.path.join(out, "README.txt")
    if os.path.exists(index) and not a.force:
        sys.stdout.write(open(index, encoding="utf-8").read())
        return
    shutil.rmtree(out, ignore_errors=True)
    os.makedirs(out)
    with tempfile.TemporaryDirectory() as tmp:
        r = subprocess.run([YTDLP, "--no-warnings", "-f", "b[height<=480]/bv*[height<=480]+ba/b",
                            "--print", "after_move:%(title)s\t%(duration)s\t%(channel)s",
                            "-o", os.path.join(tmp, "v.%(ext)s"), url], capture_output=True, text=True, timeout=3600)
        vids = sorted(glob.glob(os.path.join(tmp, "v.*")))
        if not vids:
            sys.exit("gary watch: the video download failed:\n" + (r.stderr or r.stdout)[-800:])
        title, dur, chan = (r.stdout.strip().split("\n")[-1].split("\t") + ["", "", ""])[:3]
        every = a.every
        if every is None:
            try:
                span = (end or float(dur)) - (start or 0)
            except ValueError:
                span = 600
            every = max(1.0, min(10.0, span / 60))
        pairs = frames(vids[0], os.path.join(out, "frames"), every, a.scene, a.max_frames, start, end)
        made = sheets(pairs, out)
        cues, source = captions(url, tmp)
        if not cues:
            cues, source = whisper_cues(vids[0], a.model)
    if start is not None or end is not None:
        cues = [c for c in cues if (start is None or c[0] >= start) and (end is None or c[0] <= end)]
    with open(os.path.join(out, "transcript.txt"), "w", encoding="utf-8") as f:
        for t, line in cues:
            f.write(f"[{clock(t)}] {line}\n")
    try:
        length = clock(float(dur))
    except ValueError:
        length = "?"
    lines = [f"# {title or vid}", f"{url} · {chan} · {length}" if chan else url,
             f"{len(pairs)} frames on {len(made)} sheets (3x3, each frame stamped with its time); "
             f"transcript: {source}, {len(cues)} lines.", "",
             "Open each sheet with the Read tool and read the transcript next to it:"]
    for t0, t1, path in made:
        lines.append(f"  {clock(t0)}-{clock(t1)}  {path}")
    lines += [f"  transcript  {os.path.join(out, 'transcript.txt')}", f"  frames      {os.path.join(out, 'frames')}/",
              "", "Zoom into a part: gary watch <url> --from 2:00 --to 3:30 --every 2"]
    text = "\n".join(lines) + "\n"
    open(index, "w", encoding="utf-8").write(text)
    sys.stdout.write(text)


if __name__ == "__main__":
    main()
