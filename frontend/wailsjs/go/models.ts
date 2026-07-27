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

