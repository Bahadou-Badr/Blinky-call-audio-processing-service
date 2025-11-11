"""
tools/noisereduce_denoise.py

Usage:
  python tools/noisereduce_denoise.py --in input.wav --out output.wav [--noise noise_sample.wav]

If --noise is provided, the script uses that file as a noise sample to compute the noise profile
(works well if you can capture a short 'silence with noise' clip). Otherwise it uses internal
noise_estimation (automatic).
"""
import argparse
import os
import soundfile as sf
import numpy as np
import noisereduce as nr


def load_wav(path):
    data, sr = sf.read(path, dtype="float32")
    # mono convert
    if data.ndim > 1:
        data = np.mean(data, axis=1)
    return data, sr


def write_wav(path, data, sr):
    sf.write(path, data.astype("float32"), sr)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--in", dest="infile", required=True, help="input wav path")
    p.add_argument("--out", dest="outfile", required=True, help="output wav path")
    p.add_argument("--noise", dest="noisefile", required=False,
                   help="optional short noise-only wav to estimate profile")
    p.add_argument("--prop-decrease", dest="prop_decrease", type=float, default=1.0,
                   help="0..1 strength (1.0 = full reduction). Lower = less aggressive.")
    args = p.parse_args()

    if not os.path.isfile(args.infile):
        print("input not found:", args.infile)
        raise SystemExit(2)

    sig, sr = load_wav(args.infile)

    if args.noisefile:
        if not os.path.isfile(args.noisefile):
            print("noise sample not found:", args.noisefile)
            raise SystemExit(2)
        noise_clip, nsr = load_wav(args.noisefile)
        if nsr != sr:
            print("warning: sample rate mismatch between input and noise sample")
    else:
        noise_clip = None

    # run spectral-gating noise reduction
    reduced = nr.reduce_noise(y=sig, sr=sr, y_noise=noise_clip, prop_decrease=args.prop_decrease)

    # clip just in case
    reduced = np.clip(reduced, -1.0, 1.0)
    write_wav(args.outfile, reduced, sr)
    print("wrote:", args.outfile)


if __name__ == "__main__":
    main()
