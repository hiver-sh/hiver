FROM node:22-alpine

RUN apk add --no-cache bash findutils grep

WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev
COPY server.ts ./

EXPOSE 3000

CMD ["npx", "tsx", "server.ts"]
