# Blinky-call-audio-processing-service

#### Single-instance Go API that processes uploaded files locally using FFmpeg filters

Implement a basic processing chain :

```bash
[Input WAV] → [Noise Reduction (afftdn)] → [Loudness Normalization (loudnorm)] → [Output WAV]
```

```bash
$ curl -v -F "file=@C:\Users\bdr\test\call_rec.wav" -F "denoise=afftdn" http://localhost:8080/process
* Host localhost:8080 was resolved.
* IPv6: ::1
* IPv4: 127.0.0.1
*   Trying [::1]:8080...
* Connected to localhost (::1) port 8080
> POST /process HTTP/1.1
> Host: localhost:8080
> User-Agent: curl/8.9.0
> Accept: */*
> Content-Length: 97303042
> Content-Type: multipart/form-data; boundary=------------------------6oAteLUOxMgQoCUuPD5kqv
> Expect: 100-continue
> 
< HTTP/1.1 100 Continue
< 
* upload completely sent off: 97303042 bytes
< HTTP/1.1 200 OK
< Content-Type: application/json
< Date: Mon, 20 Oct 2025 21:54:23 GMT
< Content-Length: 173
<
{"denoise":"afftdn","elapsed_ms":8121,"input":"storage\\input\\1760997255080262300_call_rec.wav","output":"storage\\output\\1760997255080262300_call_rec.wav_processed.wav"}     
* Connection #0 to host localhost left intact
```