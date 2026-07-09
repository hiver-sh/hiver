FROM node:22-slim

ENV IS_SANDBOX=1
WORKDIR /app

COPY package.json .
RUN npm install
COPY index.ts .

EXPOSE 3000
CMD ["npx", "tsx", "index.ts"]
