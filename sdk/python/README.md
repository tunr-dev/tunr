# tunr Python SDK

Official Python SDK for tunr tunnels.

## Installation

```bash
pip install tunr-sdk
```

## Usage

```python
from tunr import TunrClient

client = TunrClient()
tunnel = client.share(port=3000)
print(tunnel.public_url)
# ... do work ...
tunnel.close()
```
