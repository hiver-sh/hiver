FROM python:3.13-slim

ENV IS_SANDBOX=1
WORKDIR /app

COPY requirements.txt .
RUN pip install -r requirements.txt
COPY main.py .

EXPOSE 3000
CMD ["python", "main.py"]
