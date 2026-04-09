(ns jepsen.kvgo.support
  "Shared client implementations, HTTP helpers, and generator functions for
  kv-go Jepsen workloads. Leaf namespace — no dependencies on workload files.

  Two client modes:
    PinnedClient    — reads and writes go to the worker's assigned node.
                      Write failures return :info (unknown outcome).
                      Correct for healthy-cluster and crash workloads.
    BroadcastClient — reads go to the assigned node (to expose staleness).
                      Writes retry across all nodes to find the current leader.
                      Correct for partition workloads where the leader moves."
  (:require [clj-http.client :as http]
            [jepsen [client :as client]]))

;; ---------------------------------------------------------------------------
;; Configuration
;; ---------------------------------------------------------------------------

(def http-port 8080)

;; ---------------------------------------------------------------------------
;; HTTP helpers
;; ---------------------------------------------------------------------------

(defn kv-url
  "Returns the HTTP URL for a key on a given node."
  [node key]
  (str "http://" (name node) ":" http-port "/kv/" key))

(def read-opts
  "HTTP options for reads — short timeout, reads are fast or stale."
  {:socket-timeout     2000
   :connection-timeout 1000
   :throw-exceptions   false})

(def write-opts
  "HTTP options for writes — longer timeout to survive Raft commit latency."
  {:socket-timeout     5000
   :connection-timeout 1000
   :throw-exceptions   false})

(defn kv-read
  "Reads the 'jepsen' key from node. Returns an updated op."
  [node op]
  (try
    (let [resp (http/get (kv-url node "jepsen") read-opts)]
      (case (:status resp)
        200 (assoc op :type :ok :value (Long/parseLong (:body resp)))
        404 (assoc op :type :ok :value nil)
        (assoc op :type :fail :error [:unexpected-status (:status resp)])))
    (catch Exception e
      (assoc op :type :fail :error (.getMessage e)))))

(defn kv-write
  "Writes a value to the 'jepsen' key on node. Returns true on success."
  [node value]
  (try
    (let [resp (http/put (kv-url node "jepsen")
                         (merge write-opts {:body (str value)}))]
      (= 200 (:status resp)))
    (catch Exception _e false)))

;; ---------------------------------------------------------------------------
;; Generator helpers
;; ---------------------------------------------------------------------------

(defn r "Read operation." [_ _] {:type :invoke, :f :read, :value nil})
(defn w "Write operation." [_ _] {:type :invoke, :f :write, :value (rand-int 5)})

;; ---------------------------------------------------------------------------
;; Pinned client — all operations go to the worker's assigned node
;; ---------------------------------------------------------------------------
;; kv-go forwards proposals from any node to the leader internally via Raft.
;; The client does not need to know who the leader is. Reads return the local
;; state machine value. In a healthy cluster this is linearizable because all
;; writes go through the same Raft log and are applied in order on every node.

(defrecord PinnedClient [node]
  client/Client
  (open!    [this _test node] (assoc this :node node))
  (setup!   [_ _])
  (invoke!  [_ _ op]
    (case (:f op)
      :read  (kv-read node op)
      :write (if (kv-write node (:value op))
               (assoc op :type :ok)
               (assoc op :type :info :error :write-failed))))
  (teardown! [_ _])
  (close!    [_ _]))

;; ---------------------------------------------------------------------------
;; Broadcast client — reads pinned, writes retry across all nodes
;; ---------------------------------------------------------------------------

(defrecord BroadcastClient [node nodes]
  client/Client
  (open!    [this test node] (assoc this :node node :nodes (:nodes test)))
  (setup!   [_ _])
  (invoke!  [_ _ op]
    (case (:f op)
      :read  (kv-read node op)
      :write (let [deadline (+ (System/currentTimeMillis) 10000)]
               (loop []
                 (if (> (System/currentTimeMillis) deadline)
                   (assoc op :type :info :error :write-timeout)
                   (let [ok? (some #(kv-write % (:value op))
                                   (shuffle nodes))]
                     (if ok?
                       (assoc op :type :ok)
                       (recur))))))))
  (teardown! [_ _])
  (close!    [_ _]))
