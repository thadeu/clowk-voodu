ARG NODE_VERSION=24-alpine

FROM node:${NODE_VERSION} AS build
RUN echo $NODE_VERSION
WORKDIR /app
RUN corepack enable
COPY package.json pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY . .
RUN pnpm build

FROM oven/bun:alpine
WORKDIR /app
RUN apk add vim bash && bun install -g serve
COPY --from=build /app/out ./out
EXPOSE 80