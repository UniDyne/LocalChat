export namespace main {
	
	export class ChatMessage {
	    role: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	    }
	}
	export class ChatTurnMessage {
	    seq: number;
	    role: string;
	    content: string;
	    model: string;
	    mode: string;
	    pinned: boolean;
	    toolName?: string;
	    toolArgs?: string;
	    toolResult?: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatTurnMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.role = source["role"];
	        this.content = source["content"];
	        this.model = source["model"];
	        this.mode = source["mode"];
	        this.pinned = source["pinned"];
	        this.toolName = source["toolName"];
	        this.toolArgs = source["toolArgs"];
	        this.toolResult = source["toolResult"];
	    }
	}
	export class ChatTurnResult {
	    messages: ChatTurnMessage[];
	
	    static createFrom(source: any = {}) {
	        return new ChatTurnResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.messages = this.convertValues(source["messages"], ChatTurnMessage);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class MemoryModelInfo {
	    name: string;
	    revision: string;
	    bytes: number;
	    targetDir: string;
	    present: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MemoryModelInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.revision = source["revision"];
	        this.bytes = source["bytes"];
	        this.targetDir = source["targetDir"];
	        this.present = source["present"];
	    }
	}
	export class MemorySearchResponse {
	    results: memory.Result[];
	    report: memory.SearchReport;
	
	    static createFrom(source: any = {}) {
	        return new MemorySearchResponse(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.results = this.convertValues(source["results"], memory.Result);
	        this.report = this.convertValues(source["report"], memory.SearchReport);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class MemorySearchTuning {
	    bm25: number;
	    vector: number;
	    entity: number;
	    ngram: number;
	    mode: string;
	    expand?: boolean;
	    sourceTypes: string[];
	
	    static createFrom(source: any = {}) {
	        return new MemorySearchTuning(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bm25 = source["bm25"];
	        this.vector = source["vector"];
	        this.entity = source["entity"];
	        this.ngram = source["ngram"];
	        this.mode = source["mode"];
	        this.expand = source["expand"];
	        this.sourceTypes = source["sourceTypes"];
	    }
	}
	export class ToolState {
	    name: string;
	    enabled: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ToolState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.enabled = source["enabled"];
	    }
	}

}

export namespace memory {
	
	export class Progress {
	    pending: number;
	    running: string;
	    completed: number;
	    failed: number;
	
	    static createFrom(source: any = {}) {
	        return new Progress(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.pending = source["pending"];
	        this.running = source["running"];
	        this.completed = source["completed"];
	        this.failed = source["failed"];
	    }
	}
	export class Signals {
	    bm25: number;
	    vector: number;
	    entity: number;
	    ngram: number;
	
	    static createFrom(source: any = {}) {
	        return new Signals(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bm25 = source["bm25"];
	        this.vector = source["vector"];
	        this.entity = source["entity"];
	        this.ngram = source["ngram"];
	    }
	}
	export class Result {
	    chunkId: string;
	    sourceRef: string;
	    sourceType: string;
	    title: string;
	    headingPath: string;
	    text: string;
	    tokenCount: number;
	    score: number;
	    raw?: Signals;
	    normalized?: Signals;
	    expanded?: boolean;
	    via?: string;
	    depth?: number;
	
	    static createFrom(source: any = {}) {
	        return new Result(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.chunkId = source["chunkId"];
	        this.sourceRef = source["sourceRef"];
	        this.sourceType = source["sourceType"];
	        this.title = source["title"];
	        this.headingPath = source["headingPath"];
	        this.text = source["text"];
	        this.tokenCount = source["tokenCount"];
	        this.score = source["score"];
	        this.raw = this.convertValues(source["raw"], Signals);
	        this.normalized = this.convertValues(source["normalized"], Signals);
	        this.expanded = source["expanded"];
	        this.via = source["via"];
	        this.depth = source["depth"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WalkReport {
	    seeds: number;
	    discovered: number;
	    expanded: number;
	    queries: number;
	    truncatedFrontier: number;
	    truncatedVisited: boolean;
	    byKind: Record<string, number>;
	
	    static createFrom(source: any = {}) {
	        return new WalkReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seeds = source["seeds"];
	        this.discovered = source["discovered"];
	        this.expanded = source["expanded"];
	        this.queries = source["queries"];
	        this.truncatedFrontier = source["truncatedFrontier"];
	        this.truncatedVisited = source["truncatedVisited"];
	        this.byKind = source["byKind"];
	    }
	}
	export class SearchReport {
	    query: string;
	    mode: string;
	    candidates: number;
	    fromBm25: number;
	    fromVector: number;
	    fromEntity: number;
	    probeTerms: string[];
	    queryEntities: string[];
	    vectorSkipped?: string;
	    dedupedResults: number;
	    budgetTokens: number;
	    duration: number;
	    walk?: WalkReport;
	    expandedCandidates: number;
	    expandedReturned: number;
	
	    static createFrom(source: any = {}) {
	        return new SearchReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.query = source["query"];
	        this.mode = source["mode"];
	        this.candidates = source["candidates"];
	        this.fromBm25 = source["fromBm25"];
	        this.fromVector = source["fromVector"];
	        this.fromEntity = source["fromEntity"];
	        this.probeTerms = source["probeTerms"];
	        this.queryEntities = source["queryEntities"];
	        this.vectorSkipped = source["vectorSkipped"];
	        this.dedupedResults = source["dedupedResults"];
	        this.budgetTokens = source["budgetTokens"];
	        this.duration = source["duration"];
	        this.walk = this.convertValues(source["walk"], WalkReport);
	        this.expandedCandidates = source["expandedCandidates"];
	        this.expandedReturned = source["expandedReturned"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class Status {
	    embeddingsAvailable: boolean;
	    unavailableReason: string;
	    tokenCounter: string;
	    queue: Progress;
	    corpus: store.MemoryStats;
	    extractModel: string;
	    entityPass: Record<string, number>;
	    edgesByKind: Record<string, number>;
	
	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.embeddingsAvailable = source["embeddingsAvailable"];
	        this.unavailableReason = source["unavailableReason"];
	        this.tokenCounter = source["tokenCounter"];
	        this.queue = this.convertValues(source["queue"], Progress);
	        this.corpus = this.convertValues(source["corpus"], store.MemoryStats);
	        this.extractModel = source["extractModel"];
	        this.entityPass = source["entityPass"];
	        this.edgesByKind = source["edgesByKind"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace skill {
	
	export class Meta {
	    name: string;
	    description: string;
	    path: string;
	
	    static createFrom(source: any = {}) {
	        return new Meta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.description = source["description"];
	        this.path = source["path"];
	    }
	}

}

export namespace store {
	
	export class Artifact {
	    id: string;
	    sessionId: string;
	    title: string;
	    content: string;
	    contentType: string;
	    createdAt: string;
	
	    static createFrom(source: any = {}) {
	        return new Artifact(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.sessionId = source["sessionId"];
	        this.title = source["title"];
	        this.content = source["content"];
	        this.contentType = source["contentType"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class ArtifactMeta {
	    id: string;
	    title: string;
	    contentType: string;
	    createdAt: string;
	
	    static createFrom(source: any = {}) {
	        return new ArtifactMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.contentType = source["contentType"];
	        this.createdAt = source["createdAt"];
	    }
	}
	export class MemorySource {
	    id: string;
	    sourceType: string;
	    sourceRef: string;
	    sessionId: string;
	    title: string;
	    path: string;
	    contentHash: string;
	    mtime: string;
	    fileSize: number;
	    ingestedAt: string;
	    tokenCount: number;
	    entityPass: string;
	
	    static createFrom(source: any = {}) {
	        return new MemorySource(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.sourceType = source["sourceType"];
	        this.sourceRef = source["sourceRef"];
	        this.sessionId = source["sessionId"];
	        this.title = source["title"];
	        this.path = source["path"];
	        this.contentHash = source["contentHash"];
	        this.mtime = source["mtime"];
	        this.fileSize = source["fileSize"];
	        this.ingestedAt = source["ingestedAt"];
	        this.tokenCount = source["tokenCount"];
	        this.entityPass = source["entityPass"];
	    }
	}
	export class MemoryStats {
	    sources: number;
	    chunks: number;
	    embeddedChunks: number;
	    terms: number;
	    entities: number;
	    edges: number;
	    pendingEntities: number;
	    avgDocLength: number;
	    embedModel: string;
	    embedDims: number;
	
	    static createFrom(source: any = {}) {
	        return new MemoryStats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sources = source["sources"];
	        this.chunks = source["chunks"];
	        this.embeddedChunks = source["embeddedChunks"];
	        this.terms = source["terms"];
	        this.entities = source["entities"];
	        this.edges = source["edges"];
	        this.pendingEntities = source["pendingEntities"];
	        this.avgDocLength = source["avgDocLength"];
	        this.embedModel = source["embedModel"];
	        this.embedDims = source["embedDims"];
	    }
	}
	export class PlanStep {
	    seq: number;
	    content: string;
	    status: string;
	    updatedAt: string;
	
	    static createFrom(source: any = {}) {
	        return new PlanStep(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.content = source["content"];
	        this.status = source["status"];
	        this.updatedAt = source["updatedAt"];
	    }
	}
	export class Session {
	    id: string;
	    title: string;
	    createdAt: string;
	    messageCount: number;
	
	    static createFrom(source: any = {}) {
	        return new Session(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.createdAt = source["createdAt"];
	        this.messageCount = source["messageCount"];
	    }
	}
	export class StoredMessage {
	    seq: number;
	    role: string;
	    content: string;
	    model: string;
	    mode: string;
	    pinned: boolean;
	    toolName: string;
	    toolArgs: string;
	    toolResult: string;
	    time: string;
	
	    static createFrom(source: any = {}) {
	        return new StoredMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.role = source["role"];
	        this.content = source["content"];
	        this.model = source["model"];
	        this.mode = source["mode"];
	        this.pinned = source["pinned"];
	        this.toolName = source["toolName"];
	        this.toolArgs = source["toolArgs"];
	        this.toolResult = source["toolResult"];
	        this.time = source["time"];
	    }
	}

}

