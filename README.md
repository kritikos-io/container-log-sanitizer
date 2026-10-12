> THIS PROJECT IS JUST STARTING, WE DON'T HAVE A PUBLIC VERSION YET.
>
> CONTACT US VIA ISSUE IF YOU WANT TO JOIN OUR CAUSE!

# container-log-sanitizer
Real-time sensitive data redaction in Docker logs via custom logging driver and regex-based filters.

## 🚀 Overview

`container-log-sanitizer` is a custom container logging driver that enables **inline sanitization of container logs** before they are sent from the container.
It closes a significant security gap by intercepting `stdout`/`stderr` at the container level, preventing secrets and PII from ever reaching the host filesystem or external aggregators.

This tool is ideal for teams that handle sensitive data and need log redaction **without modifying application code** or relying solely on downstream tools like Filebeat, Logstash, or SIEMs.

## 🔐 Key Features

- 🔧 **Regex-based filtering**: Configure via environment variables to define patterns like API keys, emails, tokens, and more.
- 🧊 **Zero-intrusion**: Works without changing your containerized applications.
- 🪪 **Compliance-friendly**: Helps meet data handling rules (GDPR, HIPAA, SOC 2) by enforcing redaction at the earliest possible point.
- 📦 **Plug-and-play**: Lightweight and simple to integrate into existing k8s workflows.

## Redactor Configuration

The redactor package reads semicolon-separated regular expressions from `SANITIZE_PATTERNS`, trimming whitespace and ignoring empty entries. Invalid expressions return an error instead of silently disabling a rule. Patterns use Go's regular expression syntax; matching content is replaced with `***`.

Call `redactor.New(patterns)` to supply an explicit list of patterns. Empty or invalid expressions in that list are rejected. `RedactLine` handles strings and `RedactBytes` handles byte slices; both apply patterns in order and leave unmatched content unchanged.
