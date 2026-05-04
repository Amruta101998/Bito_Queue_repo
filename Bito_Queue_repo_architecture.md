# Bito_Queue_repo Architecture Documentation

## Project Overview

**Project Name:** Bito Queue Repository - Distributed Task Queuing System  
**Purpose:** Production-grade distributed task queue for asynchronous job processing and management  
**Target Users:** Backend developers, DevOps engineers, system architects  
**Primary Language:** Python  
**Framework:** Celery, Redis  
**Repository Type:** Backend Infrastructure/Microservices  

This repository implements a robust distributed task queuing system designed for handling high-volume asynchronous tasks with reliability, monitoring, and scalability.

---

## Domain Knowledge & Business Context

### Core Business Objectives

1. **Asynchronous Task Processing**
   - Decouple request-response from long-running operations
   - Process tasks in background without blocking user requests
   - Enable horizontal scaling of task workers

2. **Reliability & Fault Tolerance**
   - Ensure tasks complete even if workers crash
   - Implement retry mechanisms with exponential backoff
   - Handle failed tasks gracefully with Dead Letter Queue (DLQ)

3. **Monitoring & Observability**
   - Track task execution status and metrics
   - Monitor worker health and availability
   - Alert on failures and performance issues

4. **Performance & Scalability**
   - Process thousands of tasks concurrently
   - Minimize task latency and overhead
   - Scale workers based on workload

### Key Entities

#### 1. **Task**
- **Definition:** Unit of work to be executed asynchronously
- **Properties:**
  - Task ID (unique identifier)
  - Task name (function name)
  - Arguments and keyword arguments
  - Priority level (low, normal, high)
  - Retry count and max retries
  - Timeout duration
  - Created timestamp
  - Status (pending, processing, completed, failed)

#### 2. **Queue**
- **Definition:** Message broker storage for pending tasks
- **Types:**
  - Default queue: Standard task processing
  - Priority queue: High-priority urgent tasks
  - Batch queue: Bulk processing tasks
  - Scheduled queue: Delayed task execution

#### 3. **Worker**
- **Definition:** Process that executes tasks from queues
- **Properties:**
  - Worker ID
  - Queue assignments
  - Concurrency level (parallel tasks)
  - Health status
  - Last heartbeat timestamp
  - Processing statistics

#### 4. **Result Backend**
- **Definition:** Storage for task execution results
- **Stores:**
  - Task output/return value
  - Execution time
  - Status updates
  - Error information

#### 5. **Dead Letter Queue (DLQ)**
- **Definition:** Storage for tasks that failed permanently
- **Contains:**
  - Failed task data
  - Failure reason and stack trace
  - Retry history
  - Manual retry capability

### Business Rules & Constraints

1. **Task Execution Rules**
   - Tasks must complete within timeout period
   - Failed tasks automatically retry with exponential backoff
   - Maximum retry attempts: configurable (default: 3)
   - Tasks moved to DLQ after max retries exceeded

2. **Retry Policy**
   - First retry: 60 seconds delay
   - Second retry: 300 seconds delay (5 minutes)
   - Third retry: 900 seconds delay (15 minutes)
   - Formula: delay = base_delay * (2 ^ retry_count)

3. **Worker Management**
   - Workers must send heartbeat every 30 seconds
   - Workers marked offline after 2 missed heartbeats
   - Offline worker tasks reassigned to available workers
   - Graceful shutdown: complete current task before stopping

4. **Monitoring & Alerting**
   - Alert if task queue depth exceeds threshold
   - Alert if worker unavailable for > 5 minutes
   - Alert if DLQ receives > 10 tasks in 1 hour
   - Monitor Redis memory usage (alert at 80%)

5. **Data Retention**
   - Task results: 7 days
   - Task history: 30 days
   - DLQ items: 90 days (manual cleanup)
   - Logs: 14 days (with compression)

### Task Processing Workflows

#### Standard Task Execution Workflow
```
Client Request
    ↓
┌──────────────────────────────────┐
│ 1. Task Enqueue                  │
│    - Validate task               │
│    - Generate task ID            │
│    - Store in queue              │
│    - Return task ID              │
└──────────────────────────────────┘
    ↓
┌──────────────────────────────────┐
│ 2. Worker Pickup                 │
│    - Fetch from queue            │
│    - Lock task                   │
│    - Update status: processing   │
└──────────────────────────────────┘
    ↓
┌──────────────────────────────────┐
│ 3. Task Execution                │
│    - Run task function           │
│    - Capture output              │
│    - Handle exceptions           │
└──────────────────────────────────┘
    ↓
┌──────────────────────────────────┐
│ 4. Result Storage                │
│    - Store result/error          │
│    - Update status: completed    │
│    - Publish completion event    │
└──────────────────────────────────┘
    ↓
┌──────────────────────────────────┐
│ 5. Cleanup                       │
│    - Release lock                │
│    - Remove from queue           │
│    - Expire result after 7 days  │
└──────────────────────────────────┘
    ↓
Task Complete
```

#### Task Retry Workflow
```
Task Execution Fails
    ↓
┌──────────────────────────────────┐
│ Check Retry Count                │
│ Current: 0, Max: 3               │
└──────────────────────────────────┘
    ↓
  Retries < Max?
    ↙         ↘
  YES         NO
   ↓           ↓
Retry         Move to DLQ
(with delay)
   ↓           ↓
Wait          Log Failure
   ↓           ↓
Requeue    Alert Team
   ↓           ↓
Retry      Manual Review
   ↓
Success or
Continue Retrying
```

#### Dead Letter Queue Workflow
```
Task Fails After Max Retries
    ↓
┌──────────────────────────────────┐
│ Move to DLQ                      │
│ - Store full task context        │
│ - Record failure details         │
│ - Create alert                   │
│ - Add to monitoring dashboard    │
└──────────────────────────────────┘
    ↓
┌──────────────────────────────────┐
│ Team Review                      │
│ - Analyze failure root cause     │
│ - Fix underlying issue           │
│ - Prepare task for retry         │
└──────────────────────────────────┘
    ↓
┌──────────────────────────────────┐
│ Manual Retry                     │
│ - Re-execute task                │
│ - Verify success                 │
│ - Update monitoring              │
└──────────────────────────────────┘
    ↓
Task Resolved
```

---

## System Architecture

### High-Level Architecture Diagram

```
┌─────────────────────────────────────────────────────────────┐
│           Bito Queue - Distributed Task System              │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌──────────────────────────────────────────────────────┐  │
│  │         Client Application Layer                     │  │
│  │  - API endpoints for task submission                │  │
│  │  - Task status tracking                             │  │
│  │  - Result retrieval                                 │  │
│  └──────────────────────────────────────────────────────┘  │
│                          ↓                                  │
│  ┌──────────────────────────────────────────────────────┐  │
│  │    Task Queue Manager (Celery)                       │  │
│  │  - Task serialization/deserialization               │  │
│  │  - Queue routing and distribution                   │  │
│  │  - Retry logic and scheduling                       │  │
│  └──────────────────────────────────────────────────────┘  │
│         ↓              ↓              ↓                     │
│  ┌─────────┐   ┌──────────┐   ┌────────────┐              │
│  │ Default │   │ Priority │   │  Scheduled │              │
│  │ Queue   │   │ Queue    │   │  Queue     │              │
│  └─────────┘   └──────────┘   └────────────┘              │
│         ↓              ↓              ↓                     │
│  ┌──────────────────────────────────────────────────────┐  │
│  │    Message Broker (Redis)                            │  │
│  │  - Store task messages                              │  │
│  │  - Distribute to workers                            │  │
│  │  - Cache results                                    │  │
│  └──────────────────────────────────────────────────────┘  │
│         ↓              ↓              ↓                     │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐               │
│  │ Worker 1 │  │ Worker 2 │  │ Worker N │               │
│  │ (Pool)   │  │ (Pool)   │  │ (Pool)   │               │
│  └──────────┘  └──────────┘  └──────────┘               │
│         ↓              ↓              ↓                     │
│  ┌──────────────────────────────────────────────────────┐  │
│  │    Result Backend (Redis)                            │  │
│  │  - Store execution results                          │  │
│  │  - Track task status                                │  │
│  │  - Manage task metadata                             │  │
│  └──────────────────────────────────────────────────────┘  │
│         ↓              ↓              ↓                     │
│  ┌──────────────────────────────────────────────────────┐  │
│  │    Dead Letter Queue (DLQ)                           │  │
│  │  - Store permanently failed tasks                   │  │
│  │  - Enable manual retry                              │  │
│  │  - Support root cause analysis                      │  │
│  └──────────────────────────────────────────────────────┘  │
│         ↓              ↓              ↓                     │
│  ┌──────────────────────────────────────────────────────┐  │
│  │    Monitoring & Observability                        │  │
│  │  - Prometheus metrics                               │  │
│  │  - Grafana dashboards                               │  │
│  │  - Alert manager                                    │  │
│  │  - Structured logging (ELK)                         │  │
│  └──────────────────────────────────────────────────────┘  │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### Directory Structure

```
Bito_Queue_repo/
├── README.md                           # Project overview and setup
├── Bito_Queue_repo_architecture.md    # This file
├── requirements.txt                    # Python dependencies
├── setup.py                           # Package setup
├── .gitignore                         # Git ignore rules
│
├── config/                            # Configuration management
│   ├── __init__.py
│   ├── settings.py                    # Base settings
│   ├── celery_config.py               # Celery configuration
│   ├── redis_config.py                # Redis connection config
│   ├── logging_config.py              # Logging setup
│   └── environment.py                 # Environment variables
│
├── queue/                             # Core queue implementation
│   ├── __init__.py
│   ├── celery_app.py                  # Celery app initialization
│   ├── tasks.py                       # Task definitions
│   ├── decorators.py                  # Task decorators
│   ├── handlers.py                    # Task handlers
│   └── utils.py                       # Queue utilities
│
├── workers/                           # Worker implementation
│   ├── __init__.py
│   ├── base_worker.py                 # Base worker class
│   ├── worker_pool.py                 # Worker pool management
│   ├── worker_health.py               # Health monitoring
│   ├── worker_signals.py              # Signal handlers
│   └── worker_stats.py                # Statistics tracking
│
├── retry/                             # Retry logic
│   ├── __init__.py
│   ├── retry_policy.py                # Retry configuration
│   ├── exponential_backoff.py         # Backoff implementation
│   ├── retry_handler.py               # Retry execution
│   └── retry_storage.py               # Retry history
│
├── dlq/                               # Dead Letter Queue
│   ├── __init__.py
│   ├── dlq_manager.py                 # DLQ management
│   ├── dlq_storage.py                 # DLQ persistence
│   ├── dlq_recovery.py                # Recovery utilities
│   └── dlq_monitoring.py              # DLQ monitoring
│
├── monitoring/                        # Monitoring & observability
│   ├── __init__.py
│   ├── metrics.py                     # Prometheus metrics
│   ├── health_check.py                # Health endpoints
│   ├── logging_setup.py               # Structured logging
│   ├── alerts.py                      # Alert definitions
│   └── dashboards.py                  # Grafana dashboards
│
├── api/                               # REST API
│   ├── __init__.py
│   ├── app.py                         # Flask/FastAPI app
│   ├── routes.py                      # API endpoints
│   ├── middleware.py                  # Request middleware
│   ├── validators.py                  # Input validation
│   └── responses.py                   # Response formatting
│
├── models/                            # Data models
│   ├── __init__.py
│   ├── task.py                        # Task model
│   ├── worker.py                      # Worker model
│   ├── result.py                      # Result model
│   └── dlq_item.py                    # DLQ item model
│
├── storage/                           # Data persistence
│   ├── __init__.py
│   ├── redis_client.py                # Redis wrapper
│   ├── task_storage.py                # Task persistence
│   ├── result_storage.py              # Result persistence
│   └── dlq_storage.py                 # DLQ persistence
│
├── tests/                             # Test suite
│   ├── __init__.py
│   ├── test_tasks.py                  # Task tests
│   ├── test_workers.py                # Worker tests
│   ├── test_retry.py                  # Retry logic tests
│   ├── test_dlq.py                    # DLQ tests
│   ├── test_api.py                    # API tests
│   └── conftest.py                    # Pytest fixtures
│
├── docker/                            # Docker configuration
│   ├── Dockerfile                     # Worker container
│   ├── docker-compose.yml             # Multi-container setup
│   └── entrypoint.sh                  # Container entrypoint
│
├── scripts/                           # Utility scripts
│   ├── run_worker.py                  # Start worker
│   ├── run_api.py                     # Start API server
│   ├── monitor_queue.py               # Queue monitoring
│   ├── cleanup_dlq.py                 # DLQ cleanup
│   └── health_check.sh                # Health verification
│
└── docs/                              # Documentation
    ├── ARCHITECTURE.md                # Architecture overview
    ├── SETUP.md                       # Setup guide
    ├── API.md                         # API documentation
    ├── MONITORING.md                  # Monitoring guide
    └── TROUBLESHOOTING.md             # Troubleshooting guide
```

### Technology Stack

| Component | Technology | Version | Purpose |
|-----------|-----------|---------|---------|
| **Task Queue** | Celery | 5.3+ | Distributed task processing |
| **Message Broker** | Redis | 7.0+ | Queue storage and messaging |
| **Result Backend** | Redis | 7.0+ | Task result caching |
| **Web Framework** | Flask/FastAPI | Latest | REST API for task submission |
| **Monitoring** | Prometheus | Latest | Metrics collection |
| **Visualization** | Grafana | Latest | Dashboard creation |
| **Logging** | ELK Stack | Latest | Centralized logging |
| **Testing** | pytest | Latest | Unit and integration testing |
| **Containerization** | Docker | Latest | Container deployment |
| **Orchestration** | Kubernetes | Latest | Production deployment |

---

## Component Breakdown

### 1. Task Queue Manager (Celery)

#### Core Responsibilities
- **Task Registration:** Register task functions with Celery
- **Task Serialization:** Convert tasks to messages for transport
- **Queue Routing:** Route tasks to appropriate queues
- **Task Scheduling:** Schedule delayed and periodic tasks
- **Retry Management:** Handle task retries with backoff

#### Key Configuration
```python
# Celery Configuration
CELERY_BROKER_URL = 'redis://localhost:6379/0'
CELERY_RESULT_BACKEND = 'redis://localhost:6379/1'
CELERY_TASK_SERIALIZER = 'json'
CELERY_RESULT_SERIALIZER = 'json'
CELERY_ACCEPT_CONTENT = ['json']
CELERY_TIMEZONE = 'UTC'
CELERY_ENABLE_UTC = True

# Retry Configuration
CELERY_TASK_MAX_RETRIES = 3
CELERY_TASK_DEFAULT_RETRY_DELAY = 60
CELERY_TASK_ACKS_LATE = True
CELERY_WORKER_PREFETCH_MULTIPLIER = 4
```

### 2. Message Broker (Redis)

#### Responsibilities
- **Queue Storage:** Store pending tasks in Redis lists
- **Message Distribution:** Deliver tasks to workers
- **Atomic Operations:** Ensure task delivery guarantees
- **Data Persistence:** Optional persistence for reliability

#### Queue Types
- **Default Queue:** Standard task processing (FIFO)
- **Priority Queue:** High-priority tasks processed first
- **Scheduled Queue:** Delayed task execution
- **Batch Queue:** Bulk task processing

#### Redis Commands Used
```redis
LPUSH queue:default task_id      # Add task to queue
RPOP queue:default               # Get next task
ZADD scheduled:queue score task  # Schedule task
HSET task:metadata id metadata   # Store task info
EXPIRE result:task_id 604800     # 7-day expiration
```

### 3. Worker Pool

#### Worker Responsibilities
- **Task Execution:** Execute task functions
- **Concurrency Management:** Handle multiple tasks in parallel
- **Status Updates:** Update task status during execution
- **Error Handling:** Catch and log exceptions
- **Heartbeat:** Send periodic health signals

#### Worker Configuration
```python
# Worker pool size: 4 processes
# Concurrency per worker: 10 tasks
# Total capacity: 40 concurrent tasks

# Graceful shutdown:
# 1. Stop accepting new tasks
# 2. Complete current tasks
# 3. Acknowledge completed tasks
# 4. Shutdown worker process
```

### 4. Retry Mechanism

#### Retry Policy Implementation
```python
class RetryPolicy:
    """Exponential backoff retry strategy"""
    
    BASE_DELAY = 60  # 1 minute
    MAX_RETRIES = 3
    BACKOFF_FACTOR = 2
    
    def get_retry_delay(self, retry_count):
        """Calculate delay: 60, 120, 240 seconds"""
        return self.BASE_DELAY * (self.BACKOFF_FACTOR ** retry_count)
    
    def should_retry(self, retry_count):
        """Check if more retries available"""
        return retry_count < self.MAX_RETRIES
```

#### Retry Workflow
1. Task execution fails
2. Check retry count < MAX_RETRIES
3. Calculate delay using exponential backoff
4. Schedule retry after delay
5. Increment retry counter
6. Requeue task if retries remain
7. Move to DLQ if max retries exceeded

### 5. Dead Letter Queue (DLQ)

#### DLQ Responsibilities
- **Failed Task Storage:** Persist permanently failed tasks
- **Failure Analysis:** Store detailed failure information
- **Manual Retry:** Enable operator-initiated retries
- **Monitoring:** Alert on DLQ additions
- **Cleanup:** Manage DLQ size and retention

#### DLQ Item Structure
```python
{
    'task_id': 'abc123',
    'task_name': 'send_email',
    'args': [...],
    'kwargs': {...},
    'failure_reason': 'SMTP connection timeout',
    'stack_trace': '...',
    'retry_count': 3,
    'created_at': '2026-05-04T08:06:37Z',
    'failed_at': '2026-05-04T08:06:45Z',
    'retry_history': [...]
}
```

### 6. Result Backend

#### Result Storage
- **Task Output:** Store return value from task execution
- **Execution Time:** Track how long task took
- **Status:** Store task status (pending, processing, completed, failed)
- **Metadata:** Store task creation time, worker info, etc.

#### Result Expiration
```python
# Results stored with TTL (time-to-live)
RESULT_EXPIRES_IN = 604800  # 7 days
RESULT_COMPRESSION = True   # Compress large results
RESULT_SERIALIZER = 'json'  # JSON serialization
```

### 7. Monitoring & Observability

#### Metrics Collected
- **Queue Depth:** Number of pending tasks per queue
- **Task Rate:** Tasks processed per minute
- **Task Duration:** Average execution time
- **Worker Count:** Number of active workers
- **Worker Utilization:** % of worker capacity in use
- **Error Rate:** Failed tasks per minute
- **DLQ Size:** Number of items in DLQ

#### Health Checks
```python
# Worker health endpoint
GET /health/worker
Response: {
    'status': 'healthy',
    'active_tasks': 5,
    'processed_tasks': 1000,
    'failed_tasks': 2,
    'uptime_seconds': 3600
}

# Queue health endpoint
GET /health/queue
Response: {
    'queue_depth': 150,
    'workers_available': 4,
    'avg_task_duration_ms': 250,
    'dlq_size': 3
}
```

---

## Data Flow & Workflows

### Task Submission to Completion Flow

```
Client Application
    ↓
┌─────────────────────────────────────┐
│ 1. Submit Task via API              │
│    POST /api/tasks                  │
│    Body: {task_name, args, kwargs}  │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ 2. Validate Task                    │
│    - Check task_name exists         │
│    - Validate arguments             │
│    - Check queue availability       │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ 3. Serialize Task                   │
│    - Convert to JSON                │
│    - Add metadata                   │
│    - Generate task ID               │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ 4. Enqueue to Redis                 │
│    - LPUSH to queue list            │
│    - Store task metadata            │
│    - Set result TTL                 │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ 5. Return to Client                 │
│    - Return task_id                 │
│    - Provide status URL             │
│    - HTTP 202 Accepted              │
└─────────────────────────────────────┘
    ↓
Client receives task_id
(Can poll for status)
    ↓
┌─────────────────────────────────────┐
│ 6. Worker Picks Up Task             │
│    - RPOP from queue                │
│    - Lock task (prevent duplicates) │
│    - Update status: processing      │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ 7. Execute Task                     │
│    - Deserialize arguments          │
│    - Call task function             │
│    - Capture output/exception       │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ 8. Store Result                     │
│    - Save to result backend         │
│    - Update status: completed       │
│    - Set expiration (7 days)        │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ 9. Acknowledge Task                 │
│    - Remove from queue              │
│    - Release lock                   │
│    - Update metrics                 │
└─────────────────────────────────────┘
    ↓
Client polls status endpoint
    ↓
┌─────────────────────────────────────┐
│ 10. Retrieve Result                 │
│     GET /api/tasks/{task_id}        │
│     Response: {status, result}      │
└─────────────────────────────────────┘
    ↓
Client gets result
```

### Failure & Retry Flow

```
Task Execution Fails
    ↓
┌─────────────────────────────────────┐
│ Capture Exception                   │
│ - Error type                        │
│ - Error message                     │
│ - Stack trace                       │
│ - Execution time                    │
└─────────────────────────────────────┘
    ↓
┌─────────────────────────────────────┐
│ Check Retry Eligibility             │
│ - Retry count < max                 │
│ - Retryable error type              │
│ - Not in DLQ already                │
└─────────────────────────────────────┘
    ↓
    ├─ Retry Eligible ──────────┐
    │                           │
    ↓                           ↓
Calculate Delay           Move to DLQ
(exponential backoff)      (permanent failure)
    ↓                           ↓
Schedule Retry            Log Failure
(in scheduled queue)       Alert Team
    ↓                           ↓
Wait for Delay            Manual Review
    ↓                       & Retry
Requeue Task
    ↓
Retry Execution
```

---

## Testing Strategy

### Test Categories

#### 1. Unit Tests
```python
def test_task_execution_success():
    """Test successful task execution"""
    result = send_email.apply_async(
        args=('test@example.com',)
    )
    assert result.status == 'SUCCESS'
    assert result.result == {'sent': True}

def test_task_execution_failure():
    """Test task failure handling"""
    with patch('smtp_client.send') as mock:
        mock.side_effect = SMTPException('Connection failed')
        result = send_email.apply_async(
            args=('test@example.com',)
        )
        assert result.status == 'FAILURE'
        assert result.failed()
```

#### 2. Retry Tests
```python
def test_exponential_backoff():
    """Test retry delay calculation"""
    policy = RetryPolicy()
    assert policy.get_retry_delay(0) == 60
    assert policy.get_retry_delay(1) == 120
    assert policy.get_retry_delay(2) == 240

def test_max_retries_exceeded():
    """Test DLQ placement after max retries"""
    task = FailingTask.apply_async()
    # Simulate 3 failures
    for _ in range(3):
        task.retry()
    # Should be in DLQ now
    assert dlq.contains(task.id)
```

#### 3. Integration Tests
```python
def test_task_queue_workflow():
    """Test complete task workflow"""
    # Submit task
    task_id = submit_task('send_email', 'test@example.com')
    
    # Verify in queue
    assert queue.has_task(task_id)
    
    # Simulate worker pickup
    worker.process_task(task_id)
    
    # Verify result stored
    result = result_backend.get(task_id)
    assert result['status'] == 'completed'
```

#### 4. Load Tests
```python
def test_high_volume_task_submission():
    """Test handling 10,000 tasks"""
    task_ids = []
    for i in range(10000):
        task_id = submit_task('process_data', {'id': i})
        task_ids.append(task_id)
    
    # Verify all queued
    assert queue.depth() == 10000
    
    # Simulate worker processing
    start = time.time()
    for task_id in task_ids:
        worker.process_task(task_id)
    elapsed = time.time() - start
    
    # Should process 10k tasks in < 5 minutes
    assert elapsed < 300
```

### Running Tests

```bash
# Run all tests
pytest tests/

# Run with coverage
pytest --cov=queue tests/

# Run specific test file
pytest tests/test_retry.py

# Run with detailed output
pytest -v tests/

# Run performance tests
pytest tests/test_performance.py -m performance
```

---

## Custom AI Instructions

### Code Generation Guidelines

1. **Task Definition**
   - Use @task decorator from Celery
   - Include retry configuration
   - Set appropriate timeout
   - Add error handling
   - Include logging

2. **Retry Configuration**
   - Always specify max_retries
   - Use exponential backoff
   - Catch specific exceptions
   - Log retry attempts
   - Consider DLQ placement

3. **Error Handling**
   - Catch task-specific exceptions
   - Log with context (task_id, args, etc.)
   - Decide: retry or DLQ
   - Include stack trace
   - Alert on critical failures

4. **Monitoring**
   - Emit metrics for task execution
   - Track execution time
   - Monitor queue depth
   - Alert on anomalies
   - Log all significant events

### Code Detection & Violation Patterns

1. **Task Configuration Violations**
   - ❌ No max_retries specified
   - ❌ No timeout configured
   - ❌ No error handling
   - ❌ No logging
   - ✅ Complete retry and timeout configuration

2. **Retry Logic Violations**
   - ❌ Linear retry delay (should be exponential)
   - ❌ Retry all exceptions without filtering
   - ❌ No max retry limit
   - ❌ No DLQ fallback
   - ✅ Exponential backoff with max retries and DLQ

3. **Error Handling Violations**
   - ❌ Bare except clauses
   - ❌ No logging on failure
   - ❌ No context in error messages
   - ❌ Swallowing exceptions
   - ✅ Specific exception handling with detailed logging

4. **Monitoring Violations**
   - ❌ No metrics emission
   - ❌ No execution time tracking
   - ❌ No queue depth monitoring
   - ❌ No alerting on failures
   - ✅ Comprehensive monitoring and alerting

### Example: Email Task Implementation

```python
from celery import shared_task
from celery.exceptions import MaxRetriesExceededError
import logging

logger = logging.getLogger(__name__)

@shared_task(
    bind=True,
    max_retries=3,
    default_retry_delay=60,
    time_limit=300,  # 5 minutes
    soft_time_limit=280,  # 4m 40s
)
def send_email(self, email_address, subject, body):
    """
    Send email asynchronously with retry logic.
    
    Args:
        email_address: Recipient email
        subject: Email subject
        body: Email body
        
    Returns:
        dict: {'sent': True, 'message_id': '...'}
        
    Raises:
        MaxRetriesExceededError: After 3 failed attempts
    """
    try:
        logger.info(
            f"Sending email to {email_address}",
            extra={'task_id': self.request.id}
        )
        
        # Send email
        message_id = smtp_client.send(
            to=email_address,
            subject=subject,
            body=body
        )
        
        # Emit success metric
        metrics.increment('email.sent')
        
        logger.info(
            f"Email sent successfully: {message_id}",
            extra={'task_id': self.request.id}
        )
        
        return {'sent': True, 'message_id': message_id}
        
    except SMTPException as exc:
        # Retryable error
        logger.warning(
            f"SMTP error (attempt {self.request.retries}): {str(exc)}",
            extra={'task_id': self.request.id},
            exc_info=True
        )
        
        # Emit retry metric
        metrics.increment('email.retry')
        
        # Calculate backoff delay
        retry_delay = 60 * (2 ** self.request.retries)
        
        try:
            # Retry with exponential backoff
            raise self.retry(exc=exc, countdown=retry_delay)
        except MaxRetriesExceededError:
            logger.error(
                f"Max retries exceeded for email to {email_address}",
                extra={'task_id': self.request.id},
                exc_info=True
            )
            # Move to DLQ - operator must investigate
            dlq.add_task(self.request.id, {
                'task_name': self.name,
                'args': [email_address, subject, body],
                'failure_reason': str(exc),
                'retry_count': self.request.retries
            })
            raise
            
    except Exception as exc:
        # Non-retryable error
        logger.error(
            f"Unexpected error sending email: {str(exc)}",
            extra={'task_id': self.request.id},
            exc_info=True
        )
        metrics.increment('email.failed')
        # Move directly to DLQ
        dlq.add_task(self.request.id, {
            'task_name': self.name,
            'args': [email_address, subject, body],
            'failure_reason': str(exc),
            'retry_count': self.request.retries
        })
        raise
```

---

## Performance Optimization

### Optimization Strategies

#### 1. Task Optimization
- Keep task execution time < 5 minutes
- Avoid long-running operations
- Use task chains for sequential work
- Batch related tasks together

#### 2. Worker Optimization
- Configure appropriate pool size
- Set prefetch multiplier to 4
- Use gevent for I/O-bound tasks
- Monitor worker memory usage

#### 3. Queue Optimization
- Use priority queues for urgent tasks
- Separate long-running tasks to dedicated queue
- Monitor queue depth
- Scale workers based on queue depth

#### 4. Redis Optimization
- Enable persistence for critical tasks
- Configure appropriate memory limits
- Monitor Redis memory usage
- Use Redis cluster for high availability

### Performance Metrics

```
Target Metrics:
- Task submission: < 50ms
- Task pickup: < 100ms
- Average task execution: < 5s
- Queue depth: < 1000 tasks
- Worker utilization: 70-80%
- Task success rate: > 99.5%
```

---

## Monitoring & Observability

### Key Metrics

```python
# Queue metrics
metrics.gauge('queue.depth', queue.size())
metrics.gauge('queue.workers_available', worker_count)

# Task metrics
metrics.increment('task.submitted')
metrics.increment('task.completed')
metrics.increment('task.failed')
metrics.histogram('task.duration_ms', duration)

# Worker metrics
metrics.gauge('worker.active_tasks', active_count)
metrics.gauge('worker.total_processed', processed_count)
metrics.increment('worker.started')
metrics.increment('worker.shutdown')

# DLQ metrics
metrics.gauge('dlq.size', dlq.count())
metrics.increment('dlq.added')
```

### Health Checks

```bash
# Check Redis connectivity
redis-cli ping

# Check Celery worker status
celery -A queue.celery_app inspect active

# Check queue depth
celery -A queue.celery_app inspect reserved

# Monitor task processing
celery -A queue.celery_app events

# Check DLQ status
curl http://localhost:5000/health/dlq
```

### Alerting Rules

```yaml
# Alert if queue depth > 1000
alert: HighQueueDepth
expr: queue_depth > 1000
for: 5m

# Alert if task failure rate > 5%
alert: HighTaskFailureRate
expr: rate(task_failed[5m]) / rate(task_total[5m]) > 0.05
for: 5m

# Alert if worker unavailable
alert: WorkerUnavailable
expr: worker_available == 0
for: 2m

# Alert if DLQ has items
alert: DLQNotEmpty
expr: dlq_size > 0
for: 10m
```

---

## Development Workflow

### Local Setup

```bash
# Clone repository
git clone https://github.com/Amruta101998/Bito_Queue_repo.git
cd Bito_Queue_repo

# Create virtual environment
python3 -m venv venv
source venv/bin/activate

# Install dependencies
pip install -r requirements.txt

# Start Redis
redis-server

# Start Celery worker (in separate terminal)
celery -A queue.celery_app worker --loglevel=info

# Start API server (in separate terminal)
python -m api.app

# Run tests
pytest tests/
```

### Development Process

1. **Create feature branch**
   ```bash
   git checkout -b feature/new-task
   ```

2. **Implement task with retry logic**
   - Write task function
   - Add retry configuration
   - Include error handling
   - Add logging

3. **Write tests**
   - Test success case
   - Test failure and retry
   - Test DLQ placement
   - Test metrics

4. **Verify quality**
   ```bash
   pytest tests/ --cov
   pylint queue/
   flake8 queue/
   ```

5. **Commit and push**
   ```bash
   git add .
   git commit -m "Add: New task with retry logic"
   git push origin feature/new-task
   ```

---

## Contributing Guidelines

### Code Standards

1. **Task Configuration**
   - Always specify max_retries
   - Set appropriate timeout
   - Use bind=True for retry access
   - Include detailed docstring

2. **Error Handling**
   - Catch specific exceptions
   - Log with task context
   - Decide: retry or DLQ
   - Include stack traces

3. **Monitoring**
   - Emit metrics for all tasks
   - Track execution time
   - Log significant events
   - Alert on failures

4. **Testing**
   - Test success and failure paths
   - Test retry logic
   - Test DLQ placement
   - Minimum 90% coverage

### Pull Request Checklist

- [ ] Task has retry configuration
- [ ] Error handling implemented
- [ ] Metrics emitted
- [ ] Tests cover success/failure/retry
- [ ] Coverage >= 90%
- [ ] Docstrings complete
- [ ] Commit messages descriptive

---

## Resources & References

### Celery Documentation
- **Official Docs:** https://docs.celeryproject.io/
- **Task Guide:** https://docs.celeryproject.io/en/stable/userguide/tasks/
- **Retry Guide:** https://docs.celeryproject.io/en/stable/userguide/tasks/retry/

### Redis Documentation
- **Official Docs:** https://redis.io/documentation
- **Commands:** https://redis.io/commands/

### Monitoring
- **Prometheus:** https://prometheus.io/docs/
- **Grafana:** https://grafana.com/docs/
- **ELK Stack:** https://www.elastic.co/guide/

### Books & Articles
- "Celery Best Practices" - Distributed Task Queuing
- "Redis in Action" - Data structure operations
- "Designing Data-Intensive Applications" - System design

---

## FAQ

**Q: Why use exponential backoff?**  
A: Prevents overwhelming failing services. Gives time for recovery.

**Q: When should I use DLQ?**  
A: After max retries. Requires manual investigation and retry.

**Q: How do I monitor queue depth?**  
A: Use Prometheus metrics or Celery inspect commands.

**Q: What's the maximum task execution time?**  
A: Default 5 minutes. Configure based on task type.

---

## License & Attribution

This repository is maintained for distributed task processing in production systems.

**Last Updated:** 2026-05-04  
**Version:** 1.0.0  
**Maintainer:** Amruta
