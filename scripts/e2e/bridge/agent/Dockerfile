# The e2e hosted agent: a tiny HTTP server that, on a signed trigger, opens
# read-only access to the customer's ERP with its own identity and records
# the result in its persistent volume.
FROM alpine:3.20
RUN apk add --no-cache python3 postgresql16-client
COPY agent.py /agent.py
CMD ["python3", "-u", "/agent.py"]
