# Python Application Example

Deploy a Python application with Gokku using Docker.

## Basic Flask App

### Project Structure

```
my-flask-app/
├── app.py
├── requirements.txt
├── .tool-versions
└── gokku.yml
```

### gokku.yml

```yaml
apps:
  flask-app:
    lang: python
    path: .
    entrypoint: app.py
```

### app.py

```python
import os
from flask import Flask, jsonify

app = Flask(__name__)

@app.route('/')
def hello():
    return jsonify({"message": "Hello from Gokku!"})

@app.route('/health')
def health():
    return jsonify({"status": "healthy"})

if __name__ == '__main__':
    port = int(os.getenv('PORT', 8080))
    app.run(host='0.0.0.0', port=port)
```

### requirements.txt

```
flask==3.0.0
gunicorn==21.2.0
```

### Deploy

```bash
# Add remote
gokku remote add flask-app-production ubuntu@server

# Deploy (auto-setup happens on first push)
git push flask-app-production main

# Or use CLI
gokku deploy -a flask-app-production
```

## With Gunicorn (Production)

### app.py

```python
from flask import Flask, jsonify

app = Flask(__name__)

@app.route('/')
def hello():
    return jsonify({"message": "Hello from Gokku!"})

if __name__ == '__main__':
    # This only runs for local development
    app.run(host='0.0.0.0', port=8080, debug=True)
```

### gokku.yml

```yaml
apps:
  flask-app:
    lang: python
    path: .
    entrypoint: app.py
```

### Custom Dockerfile

Create `Dockerfile`:

```dockerfile
FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

EXPOSE ${PORT:-8080}

CMD ["gunicorn", "--bind", "0.0.0.0:${PORT:-8080}", "--workers", "4", "app:app"]
```

Update `gokku.yml`:

```yaml
apps:
  flask-app:
    lang: python
    path: .
      dockerfile: ./Dockerfile
```

## FastAPI Application

### main.py

```python
import os
from fastapi import FastAPI

app = FastAPI()

@app.get("/")
def read_root():
    return {"message": "Hello from Gokku!"}

@app.get("/health")
def health():
    return {"status": "healthy"}

@app.get("/items/{item_id}")
def read_item(item_id: int, q: str = None):
    return {"item_id": item_id, "q": q}
```

### requirements.txt

```
fastapi==0.104.0
uvicorn[standard]==0.24.0
```

### gokku.yml

```yaml
apps:
  fastapi-app:
    lang: python
    path: .
    entrypoint: main.py
```

### Dockerfile

```dockerfile
FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

EXPOSE ${PORT:-8080}

CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "${PORT:-8080}"]
```

## With Database

### Environment Variables

```bash
# On server
cd /opt/gokku
gokku config set DATABASE_URL="postgresql://user:pass@localhost/db" --app flask-app --env production
```

### app.py

```python
import os
from flask import Flask, jsonify
from flask_sqlalchemy import SQLAlchemy

app = Flask(__name__)
app.config['SQLALCHEMY_DATABASE_URI'] = os.getenv('DATABASE_URL')
db = SQLAlchemy(app)

class User(db.Model):
    id = db.Column(db.Integer, primary_key=True)
    name = db.Column(db.String(80), nullable=False)

@app.route('/users')
def get_users():
    users = User.query.all()
    return jsonify([{"id": u.id, "name": u.name} for u in users])

if __name__ == '__main__':
    port = int(os.getenv('PORT', 8080))
    app.run(host='0.0.0.0', port=port)
```

### requirements.txt

```
flask==3.0.0
flask-sqlalchemy==3.1.1
psycopg2-binary==2.9.9
```


## Machine Learning Service

### Project Structure

```
ml-service/
├── model.py
├── server.py
├── requirements.txt
├── .tool-versions
└── gokku.yml
```

### .tool-versions

```
python 3.11
```

### requirements.txt

```
fastapi==0.104.0
uvicorn[standard]==0.24.0
transformers==4.35.0
torch==2.1.0
```

### server.py

```python
from fastapi import FastAPI
from transformers import pipeline

app = FastAPI()

# Load model on startup
classifier = pipeline("sentiment-analysis")

@app.get("/")
def read_root():
    return {"message": "ML Service"}

@app.post("/predict")
def predict(text: str):
    result = classifier(text)
    return {"prediction": result}
```

### gokku.yml

```yaml
apps:
  ml-service:
    lang: python
    image: python:3.11
    path: .
    entrypoint: server.py
```

## With FFmpeg (Audio Processing)

### .tool-versions

```
python 3.11
ffmpeg 8.0
```

### gokku.yml

```yaml
apps:
  audio-service:
    lang: python
    path: .
    entrypoint: server.py
```

Gokku will use the system ffmpeg installation!

### server.py

```python
import os
import subprocess
from fastapi import FastAPI, UploadFile

app = FastAPI()

@app.post("/convert")
async def convert_audio(file: UploadFile):
    input_path = f"/tmp/{file.filename}"
    output_path = f"/tmp/output.mp3"
    
    # Save uploaded file
    with open(input_path, "wb") as f:
        f.write(await file.read())
    
    # Convert using ffmpeg
    subprocess.run([
        "ffmpeg", "-i", input_path,
        "-codec:a", "libmp3lame",
        output_path
    ])
    
    return {"status": "converted"}
```

## Background Worker (Celery)

### worker.py

```python
from celery import Celery

app = Celery('tasks', broker='redis://localhost:6379')

@app.task
def process_video(video_id):
    # Process video
    return f"Processed {video_id}"
```

### gokku.yml

```yaml
apps:
  web:
    lang: python
    path: .
    entrypoint: app.py
  
  worker:
    lang: python
    path: .
    entrypoint: worker.py
```

### Dockerfile for Worker

```dockerfile
FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

CMD ["celery", "-A", "worker", "worker", "--loglevel=info"]
```

## Multiple Environments

### gokku.yml

```yaml
apps:
  flask-app:
    lang: python
    path: .
    entrypoint: app.py
```

## Troubleshooting

### View Docker Logs

```bash
# Using CLI
gokku logs -a flask-app-production -f

# Or directly
ssh ubuntu@server "docker logs -f flask-app-blue"
```

### Check Container Status

```bash
# Using CLI
gokku status -a flask-app-production

# Or directly
ssh ubuntu@server "docker ps | grep flask-app"
```

### Restart Container

```bash
# Using CLI
gokku restart -a flask-app-production

# Or directly
ssh ubuntu@server "docker restart flask-app-blue"
```

### Access Container Shell

```bash
# Direct access
ssh ubuntu@server "docker exec -it flask-app-blue /bin/bash"
```

## Complete Example

Full working example: [github.com/thadeu/gokku-examples/python-flask](https://github.com/thadeu/gokku-examples/tree/main/python-flask)

## Next Steps

- [Docker Support](/guide/docker) - Advanced Docker configuration
- [Environment Variables](/guide/env-vars) - Configure your app

