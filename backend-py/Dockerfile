FROM python:3.13-slim-bookworm AS cpu
ENV PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PYTHONDONTWRITEBYTECODE=1
RUN apt-get update && apt-get install -y --no-install-recommends \
    gdal-bin libexpat1 libgomp1 ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/platform/backend-py
COPY backend-py/requirements.lock.txt ./
RUN pip install -r requirements.lock.txt && pip check
COPY backend-py/app ./app
COPY model-runtime/src /opt/platform/model-runtime/src
COPY model-runtime/model /opt/platform/model-runtime/model
ENV CHANGE_DEPLOYMENT_DIR=/opt/platform/model-runtime \
    CHANGE_ONNX_PYTHON=/usr/local/bin/python CHANGE_ONNX_PROVIDER=cpu
EXPOSE 8090
CMD ["python", "-m", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8090"]

FROM cpu AS gpu
COPY model-runtime/requirements.gpu.txt /tmp/requirements.gpu.txt
RUN pip uninstall -y onnxruntime && pip install -r /tmp/requirements.gpu.txt && pip check
ENV CHANGE_ONNX_PROVIDER=cuda
