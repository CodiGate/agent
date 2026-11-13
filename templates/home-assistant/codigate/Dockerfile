ARG BUILD_FROM
FROM ghcr.io/codigate/agent:latest AS agent

FROM ${BUILD_FROM}
COPY --from=agent /usr/local/bin/codigate-agent /usr/bin/codigate-agent
COPY run.sh /run.sh
RUN chmod a+x /run.sh
CMD ["/run.sh"]
