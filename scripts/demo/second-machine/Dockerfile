# A second machine for the golden path's step 7 (session fork): a small
# linux/arm64 box with git and the wayglass binary, nothing else. run.sh
# cross-compiles the binary into this directory's build context first.
#
# No credentials are baked in. run.sh passes WAYGLASS_URL and the agent's
# own key (minted for this run, revoked when the container exits) by name
# from its environment.
FROM alpine:3.20

RUN apk add --no-cache git ca-certificates bash jq \
 && adduser -D -h /home/agent agent

COPY wayglass /usr/local/bin/wayglass

USER agent
WORKDIR /home/agent
ENV WAYGLASS_CONFIG_DIR=/run/wayglass
# Identity in the audit log is the name the agent key is bound to
# (run.sh: WAYGLASS_AGENT, default agent@<hostname>).
CMD ["bash"]
