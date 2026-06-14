# One-shot RisingWave schema applier. Bakes apply.sh + the full SQL tree so
# it runs as a Nomad batch job (no repo bind-mount on the node).
FROM postgres:17
WORKDIR /work/risingwave
COPY dataplane/risingwave/apply.sh ./apply.sh
COPY dataplane/risingwave/schemas ./schemas
COPY dataplane/risingwave/migrations ./migrations
COPY dataplane/risingwave/plugins ./plugins
RUN chmod +x ./apply.sh
ENTRYPOINT ["bash", "./apply.sh"]
