# One-shot RisingWave schema applier. Bakes apply.sh + the full SQL tree so
# it runs as a Nomad batch job (no repo bind-mount on the node).
FROM postgres:17
WORKDIR /work/risingwave
COPY infra/risingwave/apply.sh ./apply.sh
COPY infra/risingwave/schemas ./schemas
COPY infra/risingwave/migrations ./migrations
COPY infra/risingwave/plugins ./plugins
RUN chmod +x ./apply.sh
ENTRYPOINT ["bash", "./apply.sh"]
